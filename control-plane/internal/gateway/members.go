package gateway

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"garrison/control-plane/internal/agents"
	"garrison/control-plane/internal/events"
	"garrison/control-plane/internal/hooks"
	"garrison/templates"
)

// Members keeps each unit's config repo: who may change the unit,
// as members.yaml there says, and the checks on its agents. For garrison,
// which has no members.yaml, only root admins approve changes to config.
// Who may change a unit, as members.yaml says:
//
//	admins: [alice]   # approve and merge changes, including to members.yaml
//	members: [bob]    # push to the unit's repos
//
// Everyone signed in may read everything. The file is the truth: Gitea's teams
// "admins" and "members" are kept in line with it. It changes only through
// pull requests, which need the approval of an admin of the unit or a
// root admin who neither wrote the pull request (opened it or pushed to its
// branch) nor has their role changed by it. Gitea cannot check that, so
// this posts a required status, garrison/approval, on every open pull
// request of config.
type Members struct {
	// Wake, when set, starts a sync before the interval is over.
	Wake   <-chan struct{}
	Gitea  *Gitea
	Events events.Recorder // the feed, told when a unit is set up; nil: none
	// Keycloak, when set, gives each agent account one of the same name
	// there, enabled while the Gitea account is unlocked.
	Keycloak *Keycloak
	// RetryWait is the pause before a config repo's files are tried again;
	// 0: a second.
	RetryWait time.Duration

	posted  map[string]string // pull request head -> status last posted
	applied map[string]string // agent account -> the repos it was last given
	current map[string]string // pull request -> its head and base when last brought up to date
}

const (
	// RootTeam holds the platform's root admins in every unit, kept from
	// Gitea's site admins: Gitea counts approvals and merges only from people
	// with write access to the repo, which root admins otherwise lack.
	RootTeam        = "root-admins"
	ConfigRepo      = "config"
	MembersFile     = "members.yaml"
	ApprovalContext = "garrison/approval"
)

// Roster is the content of members.yaml.
type Roster struct {
	Admins  []string `yaml:"admins"`
	Members []string `yaml:"members"`
}

// ParseRoster reads members.yaml and checks it.
func ParseRoster(data []byte) (Roster, error) {
	var r Roster
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err := dec.Decode(&r); err != nil {
		return r, fmt.Errorf("members.yaml: %w", err)
	}
	seen := map[string]string{}
	for role, names := range map[string][]string{"admins": r.Admins, "members": r.Members} {
		for _, n := range names {
			if !userName.MatchString(n) {
				return r, fmt.Errorf("members.yaml: %q is not a username", n)
			}
			if other, dup := seen[n]; dup {
				return r, fmt.Errorf("members.yaml: %s is listed under both %s and %s", n, other, role)
			}
			seen[n] = role
		}
	}
	if len(r.Admins) == 0 {
		return r, errors.New("members.yaml: a unit needs at least one admin")
	}
	return r, nil
}

// Role is a person's role in a roster: admin, member or "".
func (r Roster) Role(name string) string {
	switch {
	case slices.Contains(r.Admins, name):
		return "admin"
	case slices.Contains(r.Members, name):
		return "member"
	}
	return ""
}

// Only is the roster's one person, if it has exactly one.
func (r Roster) Only() (string, bool) {
	all := slices.Concat(r.Admins, r.Members)
	if len(all) != 1 {
		return "", false
	}
	return all[0], true
}

// Changed lists the people whose role differs between two rosters.
func Changed(before, after Roster) []string {
	var out []string
	for _, n := range slices.Concat(before.Admins, before.Members, after.Admins, after.Members) {
		if before.Role(n) != after.Role(n) && !slices.Contains(out, n) {
			out = append(out, n)
		}
	}
	slices.Sort(out)
	return out
}

// Approval decides a pull request on members.yaml: it may be merged once an
// admin of the unit (by the roster on main) or a root admin approves it
// who neither wrote it nor has their role changed by it. authors wrote it:
// who opened it, first, and everyone who pushed to its branch, as an admin
// could otherwise add to someone else's pull request and approve the result.
func Approval(base, head Roster, authors []string, approvers, rootAdmins []string) (bool, string) {
	changed := Changed(base, head)
	// A unit of one person has nobody else to approve: its person's own
	// change goes in, as long as they stay its admin.
	if only, ok := base.Only(); ok && head.Role(only) == "admin" && !slices.ContainsFunc(authors, func(a string) bool { return a != only }) {
		return true, only + " is the only person of the unit and proposed it"
	}
	var refused []string
	for _, a := range approvers {
		switch {
		case len(authors) > 0 && a == authors[0]:
			refused = append(refused, a+" wrote it")
		case slices.Contains(authors, a):
			refused = append(refused, a+" pushed to it")
		case slices.Contains(changed, a):
			refused = append(refused, a+"'s own role changes")
		case base.Role(a) != "admin" && !slices.Contains(rootAdmins, a):
			refused = append(refused, a+" is not an admin")
		default:
			return true, "approved by " + a
		}
	}
	why := "needs the approval of an admin who did not write it and whose role it does not change"
	if len(changed) > 0 {
		why += " (changes: " + strings.Join(changed, ", ") + ")"
	}
	if len(refused) > 0 {
		why += "; not counted: " + strings.Join(refused, ", ")
	}
	return false, why
}

// SoleRoot tells whether a change of garrison's config was written by its
// only root admin alone: then nobody else could approve it, as in a unit of
// one person, and it goes in. roots are the people (see people): right
// after the setup, its first root admin is the only one.
func SoleRoot(authors, roots []string) (string, bool) {
	if len(roots) != 1 || len(authors) == 0 {
		return "", false
	}
	for _, a := range authors {
		if a != roots[0] {
			return "", false
		}
	}
	return roots[0], true
}

// people are the root admins but for the platform's own account, which
// every installation has and nobody works as.
func people(roots []string, platform string) []string {
	return slices.DeleteFunc(slices.Clone(roots), func(r string) bool { return r == platform })
}

var allUnits = []string{"repo.code", "repo.issues", "repo.pulls", "repo.releases", "repo.wiki", "repo.projects", "repo.packages", "repo.actions"}

func unitsMap(access string) map[string]string {
	m := map[string]string{}
	for _, u := range allUnits {
		m[u] = access
	}
	return m
}

// Sync sets up and applies every unit's members.
func (m *Members) Sync(ctx context.Context) error {
	if m.posted == nil {
		m.posted = map[string]string{}
	}
	units, err := m.Gitea.Units(ctx, "")
	if err != nil {
		return err
	}
	users, err := m.users(ctx)
	if err != nil {
		return err
	}
	var roots []string
	for _, u := range users {
		if u.IsAdmin {
			roots = append(roots, u.Login)
		}
	}
	var errs []error
	present := map[string]bool{}
	for _, ns := range units {
		present[ns.Name] = true
		if ns.Name == PlatformUnit {
			if err := m.platform(ctx, roots); err != nil {
				errs = append(errs, fmt.Errorf("config of garrison: %w", err))
			}
		} else if err := m.unit(ctx, ns.Name, roots); err != nil {
			errs = append(errs, fmt.Errorf("members of %s: %w", ns.Name, err))
		}
		if err := m.accounts(ctx, ns.Name, users); err != nil {
			errs = append(errs, fmt.Errorf("agent accounts of %s: %w", ns.Name, err))
		}
		if err := m.keepCurrent(ctx, ns.Name); err != nil {
			errs = append(errs, fmt.Errorf("changes of %s: %w", ns.Name, err))
		}
	}
	// The agents of a unit that is gone: their accounts are locked, so
	// a unit made again under its name starts without them.
	for _, u := range users {
		rest, ok := strings.CutPrefix(u.FullName, AccountPrefix)
		ns, _, ok2 := strings.Cut(rest, "/")
		if !ok || !ok2 || present[ns] || u.ProhibitLogin {
			continue
		}
		if err := m.lock(ctx, u.Login); err != nil {
			errs = append(errs, err)
			continue
		}
		log.Printf("members: locked the account %s: its unit %s is gone", u.Login, ns)
		if err := m.keycloak(ctx, u.Login, rest, false); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// keycloak enables or disables the agent's account in Keycloak, if there
// is one to keep.
func (m *Members) keycloak(ctx context.Context, account, id string, enabled bool) error {
	if m.Keycloak == nil {
		return nil
	}
	if err := m.Keycloak.Keep(ctx, account, id, enabled); err != nil {
		return fmt.Errorf("keycloak account %s: %w", account, err)
	}
	return nil
}

// lock keeps an agent's account from acting: Gitea refuses its sign-ins
// and the gateway's Sudo as it.
func (m *Members) lock(ctx context.Context, login string) error {
	_, err := m.Gitea.do(ctx, "", http.MethodPatch, "/admin/users/"+login, map[string]any{
		"login_name": login, "source_id": 0, "prohibit_login": true,
	}, nil)
	return err
}

type giteaUser struct {
	Login         string `json:"login"`
	FullName      string `json:"full_name"`
	Email         string `json:"email"`
	IsAdmin       bool   `json:"is_admin"`
	ProhibitLogin bool   `json:"prohibit_login"`
}

func (m *Members) users(ctx context.Context) ([]giteaUser, error) { return m.Gitea.users(ctx) }

// users lists every Gitea account.
func (g *Gitea) users(ctx context.Context) ([]giteaUser, error) {
	var out []giteaUser
	for page := 1; page < 40; page++ {
		var users []giteaUser
		if _, err := g.do(ctx, "", http.MethodGet, fmt.Sprintf("/admin/users?limit=50&page=%d", page), nil, &users); err != nil {
			return nil, err
		}
		out = append(out, users...)
		if len(users) < 50 {
			break
		}
	}
	return out, nil
}

// accounts keeps the Gitea accounts of a unit's agents that have
// grants or routines: made when missing, writing to the repos their grants
// name and no others, and locked, with their access taken away, when their
// agent is gone or switched off. Nobody signs in with them: the gateway
// acts as them through Sudo.
func (m *Members) accounts(ctx context.Context, ns string, users []giteaUser) error {
	g := m.Gitea
	if m.applied == nil {
		m.applied = map[string]string{}
	}
	list, _, err := g.Agents(ctx, ns, "main")
	if err != nil {
		return err
	}
	want := map[string]agents.Agent{}
	for _, a := range list {
		if a.NeedsAccount() {
			want[AgentAccount(&a)] = a
		}
	}
	var mine []giteaUser
	for _, u := range users {
		if strings.HasPrefix(u.FullName, AccountPrefix+ns+"/") {
			mine = append(mine, u)
		}
	}
	if len(want) == 0 && len(mine) == 0 {
		return nil
	}
	repos, err := g.Repos(ctx, "", ns)
	if err != nil {
		return err
	}
	var names []string
	for _, r := range repos {
		names = append(names, r.Name)
	}
	slices.Sort(names)
	accounts := make([]string, 0, len(want))
	for name := range want {
		accounts = append(accounts, name)
	}
	slices.Sort(accounts)
	for _, name := range accounts {
		a := want[name]
		i := slices.IndexFunc(users, func(u giteaUser) bool { return u.Login == name })
		switch {
		case i < 0:
			if _, err := g.do(ctx, "", http.MethodPost, "/admin/users", map[string]any{
				"username": name, "email": name + "@agents.invalid", "full_name": AccountPrefix + a.ID(),
				"password": randomPassword(), "must_change_password": false, "send_notify": false,
			}, nil); err != nil {
				return fmt.Errorf("make %s: %w", name, err)
			}
			log.Printf("members: made the account %s of the agent %s", name, a.ID())
			delete(m.applied, name)
		case users[i].ProhibitLogin:
			if _, err := g.do(ctx, "", http.MethodPatch, "/admin/users/"+name, map[string]any{
				"login_name": name, "source_id": 0, "prohibit_login": false,
			}, nil); err != nil {
				return err
			}
			log.Printf("members: unlocked the account %s of the agent %s", name, a.ID())
			delete(m.applied, name)
		}
		if err := m.collaborations(ctx, ns, name, a.GrantedRepos(), names); err != nil {
			return err
		}
		if err := m.keycloak(ctx, name, a.ID(), true); err != nil {
			return err
		}
	}
	for _, u := range mine {
		if _, ok := want[u.Login]; ok {
			continue
		}
		if err := m.collaborations(ctx, ns, u.Login, nil, names); err != nil {
			return err
		}
		if !u.ProhibitLogin {
			if err := m.lock(ctx, u.Login); err != nil {
				return err
			}
			log.Printf("members: locked the account %s: its agent is gone, switched off or has no grants", u.Login)
		}
		if err := m.keycloak(ctx, u.Login, strings.TrimPrefix(u.FullName, AccountPrefix), false); err != nil {
			return err
		}
	}
	return nil
}

// collaborations gives account write access to the repos in granted and
// takes it away from every other repo of the unit. It is applied again
// only when either list changes.
func (m *Members) collaborations(ctx context.Context, ns, account string, granted, repos []string) error {
	key := ns + ":" + strings.Join(granted, ",") + "|" + strings.Join(repos, ",")
	if m.applied[account] == key {
		return nil
	}
	g := m.Gitea
	for _, r := range repos {
		path := fmt.Sprintf("/repos/%s/%s/collaborators/%s", ns, r, account)
		status, err := g.do(ctx, "", http.MethodGet, path, nil, nil)
		if err != nil && status != http.StatusNotFound {
			return err
		}
		has, wants := status == http.StatusNoContent, slices.Contains(granted, r)
		switch {
		case wants && !has:
			if _, err := g.do(ctx, "", http.MethodPut, path, map[string]string{"permission": "write"}, nil); err != nil {
				return err
			}
			log.Printf("members: %s may now write to %s/%s", account, ns, r)
		case !wants && has:
			if _, err := g.do(ctx, "", http.MethodDelete, path, nil, nil); err != nil {
				return err
			}
			log.Printf("members: %s may no longer write to %s/%s", account, ns, r)
		}
	}
	m.applied[account] = key
	return nil
}

func randomPassword() string {
	b := make([]byte, 24)
	rand.Read(b)
	return hex.EncodeToString(b)
}

type team struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

// teams lists a unit's teams, making the ones in want (name: access)
// that are missing.
func (m *Members) teams(ctx context.Context, ns string, want map[string]string) (map[string]int64, error) {
	var list []team
	if _, err := m.Gitea.do(ctx, "", http.MethodGet, "/orgs/"+ns+"/teams", nil, &list); err != nil {
		return nil, err
	}
	out := map[string]int64{}
	for _, t := range list {
		out[t.Name] = t.ID
	}
	for name, access := range want {
		if _, ok := out[name]; ok {
			continue
		}
		var t team
		if _, err := m.Gitea.do(ctx, "", http.MethodPost, "/orgs/"+ns+"/teams", map[string]any{
			"name": name, "description": teamDescription(name),
			"permission": access, "units_map": unitsMap(access),
			"includes_all_repositories": true, "can_create_org_repo": name == "admins",
		}, &t); err != nil {
			return nil, err
		}
		out[name] = t.ID
	}
	return out, nil
}

func teamDescription(name string) string {
	if name == RootTeam {
		return "The platform's root admins, kept by the gateway."
	}
	return "Kept from config/members.yaml; change it there."
}

// teamMembers lists a team's members: all of them, page by page, as
// syncTeam removes only the ones it sees. Gitea lists 50 at once.
func (m *Members) teamMembers(ctx context.Context, id int64) ([]string, error) {
	out := []string{}
	for page := 1; page <= 100; page++ {
		var users []struct {
			Login string `json:"login"`
		}
		if _, err := m.Gitea.do(ctx, "", http.MethodGet, fmt.Sprintf("/teams/%d/members?limit=50&page=%d", id, page), nil, &users); err != nil {
			return nil, err
		}
		for _, u := range users {
			out = append(out, u.Login)
		}
		if len(users) < 50 {
			break
		}
	}
	return out, nil
}

func (m *Members) unit(ctx context.Context, ns string, roots []string) error {
	g := m.Gitea
	teams, err := m.teams(ctx, ns, map[string]string{"admins": "admin", "members": "write", RootTeam: "admin"})
	if err != nil {
		return err
	}

	// config: created with the organisation's owners as its first admins,
	// and the operator agent.
	if status, err := g.do(ctx, "", http.MethodGet, "/repos/"+ns+"/"+ConfigRepo, nil, nil); status == http.StatusNotFound {
		owners, err := m.teamMembers(ctx, teams["Owners"])
		if err != nil {
			return err
		}
		owners = slices.DeleteFunc(owners, func(n string) bool { return n == BuilderName(ns) })
		if len(owners) == 0 {
			return errors.New("no owner to make the first admin")
		}
		data, _ := yaml.Marshal(Roster{Admins: owners, Members: []string{}})
		// templates/unit-config: members.yaml is its header.
		files := templates.Files(templates.Unit)
		files[MembersFile] += string(data)
		if err := m.createConfig(ctx, ns, "Who may change this unit, its apps, and what the agents may do here", "First admins: the unit's creators", files); err != nil {
			return err
		}
		log.Printf("members: %s/config made, admins %v", ns, owners)
		if m.Events != nil {
			m.Events.Record(ctx, events.Event{Source: events.SourceGateway, Kind: events.KindUnit, Status: events.StatusOK,
				Unit: ns, Audience: events.AudienceMembers, Actor: owners[0],
				Title:  fmt.Sprintf("The unit %s is set up: its config repo, with %s as its first admin", ns, strings.Join(owners, ", ")),
				Detail: "Its channel and its build account follow in moments. Make its repos, and propose its apps in stacks/ of its config.",
				Key:    "unit:" + ns + ":" + time.Now().UTC().Format(time.RFC3339)})
		}
	} else if err != nil {
		return err
	}
	// Apply members.yaml on main to the teams.
	var raw string
	if _, err := g.do(ctx, "", http.MethodGet, "/repos/"+ns+"/"+ConfigRepo+"/raw/"+MembersFile+"?ref=main", nil, &raw); err != nil {
		return err
	}
	roster, err := ParseRoster([]byte(raw))
	// One person approves nobody's change but their own: Gitea asks for no
	// approval, garrison/approval for theirs (see Approval).
	required := 1
	if _, alone := roster.Only(); err == nil && alone {
		required = 0
	}
	if err := m.protect(ctx, ns, []string{"admins", RootTeam}, required); err != nil {
		return err
	}
	if err != nil {
		return err // keep the teams as they are
	}
	for name, want := range map[string][]string{"admins": roster.Admins, "members": roster.Members, RootTeam: roots} {
		if err := m.syncTeam(ctx, ns, name, teams[name], want); err != nil {
			return err
		}
	}
	if err := m.teamRepos(ctx, ns, teams, "admins", "members", RootTeam); err != nil {
		return err
	}
	m.deleteMerged(ctx, ns)
	if err := m.protectApps(ctx, ns, roster); err != nil {
		return err
	}
	return m.approvals(ctx, ns, &roster, roots)
}

// teamRepos gives the teams named every repo of the unit they lack.
// They are made to include all repos, but Gitea misses one made while it
// makes the team: the team's people could not merge there, and Gitea
// leaves the team out of that repo's rule (approvals_whitelist_teams), so
// the rule would differ, and be written again, at every sync.
func (m *Members) teamRepos(ctx context.Context, ns string, teams map[string]int64, names ...string) error {
	repos, err := m.Gitea.Repos(ctx, "", ns)
	if err != nil {
		return err
	}
	for _, name := range names {
		id, ok := teams[name]
		if !ok {
			continue
		}
		var have []Repo
		if _, err := m.Gitea.do(ctx, "", http.MethodGet, fmt.Sprintf("/teams/%d/repos?limit=50", id), nil, &have); err != nil {
			return err
		}
		for _, r := range repos {
			if slices.ContainsFunc(have, func(h Repo) bool { return h.Name == r.Name }) {
				continue
			}
			if _, err := m.Gitea.do(ctx, "", http.MethodPut, fmt.Sprintf("/teams/%d/repos/%s/%s", id, ns, url.PathEscape(r.Name)), nil, nil); err != nil {
				return fmt.Errorf("%s/%s for the team %s: %w", ns, r.Name, name, err)
			}
			log.Printf("members: the team %s/%s now has %s, which Gitea had left out", ns, name, r.Name)
		}
	}
	return nil
}

// deleteMerged sets Gitea's default for deleting a pull request's branch
// when it is merged, in every repo of the unit (Gitea has no default
// for the installation): on, but off in a repo that an app deploys from a
// branch besides main, such as dev, so merging dev into main in Gitea does
// not delete dev, and its environment with it, unless the person ticks
// the box. Garrison's own merges delete the branch unless it is an
// environment's (Gitea.Merge). Repos made later, also by hand in Gitea,
// get it at the next sync. A repo it cannot set is logged and left for the
// next sync: the rest of the sync goes on.
func (m *Members) deleteMerged(ctx context.Context, ns string) {
	repos, err := m.Gitea.Repos(ctx, "", ns)
	if err != nil {
		log.Printf("members: %s: deleting merged branches: %v", ns, err)
		return
	}
	envs, err := m.Gitea.environments(ctx, ns)
	if err != nil {
		log.Printf("members: %s: deleting merged branches: %v", ns, err)
		return
	}
	keeps := map[string]bool{}
	for _, e := range envs {
		if e.Branch != "main" {
			keeps[e.Repo] = true
		}
	}
	for _, r := range repos {
		want := !keeps[r.Name]
		if r.DeleteMerged == want || r.Archived {
			continue
		}
		// Gitea changes the pull requests' settings only along with has_pull_requests.
		if _, err := m.Gitea.do(ctx, "", http.MethodPatch, fmt.Sprintf("/repos/%s/%s", ns, url.PathEscape(r.Name)),
			map[string]bool{"has_pull_requests": true, "default_delete_branch_after_merge": want}, nil); err != nil {
			log.Printf("members: %s/%s: deleting merged branches: %v", ns, r.Name, err)
			continue
		}
		if want {
			log.Printf("members: %s/%s now deletes a pull request's branch when it is merged", ns, r.Name)
		} else {
			log.Printf("members: %s/%s keeps a pull request's branch when it is merged: an app deploys its branches", ns, r.Name)
		}
	}
}

// platform keeps garrison/config, which holds the agents, for every
// unit. It has no members.yaml: root admins approve and merge its
// changes.
func (m *Members) platform(ctx context.Context, roots []string) error {
	ns := PlatformUnit
	teams, err := m.teams(ctx, ns, map[string]string{RootTeam: "admin"})
	if err != nil {
		return err
	}
	if status, err := m.Gitea.do(ctx, "", http.MethodGet, "/repos/"+ns+"/"+ConfigRepo, nil, nil); status == http.StatusNotFound {
		if err := m.createConfig(ctx, ns, "Garrison's agents, for every unit; root admins approve changes", "The operator and the commander", agents.PlatformFiles()); err != nil {
			return err
		}
		log.Printf("members: %s/config made", ns)
	} else if err != nil {
		return err
	}
	// The only root admin has nobody to approve their change: Gitea asks
	// for no approval then, garrison/approval for theirs (see SoleRoot).
	required := 1
	if len(people(roots, m.Gitea.User)) == 1 {
		required = 0
	}
	if err := m.protect(ctx, ns, []string{RootTeam}, required); err != nil {
		return err
	}
	if err := m.syncTeam(ctx, ns, RootTeam, teams[RootTeam], roots); err != nil {
		return err
	}
	m.deleteMerged(ctx, ns)
	return m.approvals(ctx, ns, nil, roots)
}

// createConfig makes a unit's config repo with files, in one commit.
// The repo is made empty, without a branch, so nobody can branch off it
// until the commit with every file is in. The repo and its files are two
// requests: should the files not go in (a
// push by someone else in between makes Gitea refuse it, as the model list
// of garrison/config did once), the repo goes again, so the next pass makes
// it anew instead of finding it there and taking it as set up.
func (m *Members) createConfig(ctx context.Context, ns, description, message string, files map[string]string) error {
	g := m.Gitea
	if _, err := g.do(ctx, "", http.MethodPost, "/orgs/"+ns+"/repos", map[string]any{
		"name": ConfigRepo, "description": description,
		"auto_init": false, "default_branch": "main", "private": false,
	}, nil); err != nil {
		return err
	}
	var ops []map[string]string
	for _, path := range sortedKeys(files) {
		ops = append(ops, map[string]string{"operation": "create", "path": path, "content": b64(files[path])})
	}
	var err error
	for try := range 3 {
		if try > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(m.retryWait()):
			}
		}
		if _, err = g.do(ctx, "", http.MethodPost, "/repos/"+ns+"/"+ConfigRepo+"/contents", map[string]any{
			"branch": "main", "message": message, "files": ops,
		}, nil); err == nil {
			return nil
		}
	}
	if _, derr := g.do(context.WithoutCancel(ctx), "", http.MethodDelete, "/repos/"+ns+"/"+ConfigRepo, nil, nil); derr != nil {
		return fmt.Errorf("%w; and %s/config, made without its files, stays: %v", err, ns, derr)
	}
	return fmt.Errorf("%s/config without its files, taken away again: %w", ns, err)
}

func (m *Members) retryWait() time.Duration {
	if m.RetryWait > 0 {
		return m.RetryWait
	}
	return time.Second
}

// syncTeam makes a team's members want. People without a Gitea account yet
// are added after their first sign-in.
func (m *Members) syncTeam(ctx context.Context, ns, name string, id int64, want []string) error {
	g := m.Gitea
	have, err := m.teamMembers(ctx, id)
	if err != nil {
		return err
	}
	for _, u := range want {
		if slices.Contains(have, u) {
			continue
		}
		if status, err := g.do(ctx, "", http.MethodPut, fmt.Sprintf("/teams/%d/members/%s", id, url.PathEscape(u)), nil, nil); err != nil {
			if status == http.StatusNotFound {
				continue
			}
			return err
		}
		log.Printf("members: %s is now in %s/%s", u, ns, name)
	}
	for _, u := range have {
		if slices.Contains(want, u) {
			continue
		}
		if _, err := g.do(ctx, "", http.MethodDelete, fmt.Sprintf("/teams/%d/members/%s", id, url.PathEscape(u)), nil, nil); err != nil {
			return err
		}
		log.Printf("members: %s is no longer in %s/%s", u, ns, name)
	}
	return nil
}

// protect keeps config's main branch changing only by approved pull requests.
// mergers are the teams that approve and merge. Only the platform's own
// account pushes there: the dashboard's secrets form writes the encrypted
// secrets an admin sets (secrets.Store), which have no diff to review.
func (m *Members) protect(ctx context.Context, ns string, mergers []string, approvals int) error {
	rule := map[string]any{
		"rule_name":                    "main",
		"enable_push":                  true,
		"enable_push_whitelist":        true,
		"push_whitelist_usernames":     []string{m.Gitea.User},
		"push_whitelist_teams":         []string{},
		"push_whitelist_deploy_keys":   false,
		"enable_merge_whitelist":       true,
		"merge_whitelist_teams":        mergers,
		"merge_whitelist_usernames":    []string{},
		"enable_approvals_whitelist":   true,
		"approvals_whitelist_teams":    mergers,
		"approvals_whitelist_username": []string{},
		"required_approvals":           approvals,
		"enable_status_check":          true,
		"status_check_contexts":        []string{ApprovalContext},
		"block_on_rejected_reviews":    true,
		"dismiss_stale_approvals":      true,
		"block_admin_merge_override":   true,
	}
	return m.setRule(ctx, ns, ConfigRepo, rule)
}

// setRule makes the protection rule of a repo's main what rule says; nil
// removes it.
func (m *Members) setRule(ctx context.Context, ns, repo string, rule map[string]any) error {
	path := "/repos/" + ns + "/" + repo + "/branch_protections"
	var have map[string]any
	status, err := m.Gitea.do(ctx, "", http.MethodGet, path+"/main", nil, &have)
	switch {
	case status == http.StatusNotFound && rule == nil:
		return nil
	case status == http.StatusNotFound:
		_, err := m.Gitea.do(ctx, "", http.MethodPost, path, rule, nil)
		return err
	case err != nil:
		return err
	case rule == nil:
		_, err := m.Gitea.do(ctx, "", http.MethodDelete, path+"/main", nil, nil)
		return err
	}
	// Written only when it differs: every change of a rule makes Gitea check
	// the branch's open pull requests again, and refuse merges meanwhile.
	if sameRule(rule, have) {
		return nil
	}
	rule = maps.Clone(rule)
	delete(rule, "rule_name")
	_, err = m.Gitea.do(ctx, "", http.MethodPatch, path+"/main", rule, nil)
	return err
}

// protectApps keeps main of a unit's other repos, its apps': a
// unit of one person pushes and merges there freely; one of more
// changes main only by pull requests another of its people approved, so
// nobody deploys alone what the others have not seen. The platform owns
// this rule: one set by hand is replaced.
func (m *Members) protectApps(ctx context.Context, ns string, roster Roster) error {
	people := map[string]bool{}
	for _, p := range slices.Concat(roster.Admins, roster.Members) {
		people[p] = true
	}
	var rule map[string]any
	if len(people) > 1 {
		rule = map[string]any{
			"rule_name":                    "main",
			"enable_push":                  false,
			"enable_merge_whitelist":       false,
			"enable_approvals_whitelist":   true,
			"approvals_whitelist_teams":    []string{"admins", "members", RootTeam},
			"approvals_whitelist_username": []string{},
			"required_approvals":           1,
			"block_on_rejected_reviews":    true,
			"dismiss_stale_approvals":      true,
			"block_admin_merge_override":   true,
		}
	}
	repos, err := m.Gitea.Repos(ctx, "", ns)
	if err != nil {
		return err
	}
	var errs []error
	for _, r := range repos {
		if r.Name == ConfigRepo {
			continue
		}
		if err := m.setRule(ctx, ns, r.Name, rule); err != nil {
			errs = append(errs, fmt.Errorf("%s/%s: %w", ns, r.Name, err))
		}
	}
	return errors.Join(errs...)
}

// sameRule tells whether have, as Gitea returns it, already holds every
// setting of want. Lists compare as sets.
func sameRule(want, have map[string]any) bool {
	for k, w := range want {
		h, ok := have[k]
		if !ok {
			return false
		}
		if list, isList := w.([]string); isList {
			got, _ := h.([]any)
			if len(got) != len(list) {
				return false
			}
			for _, x := range got {
				if s, _ := x.(string); !slices.Contains(list, s) {
					return false
				}
			}
			continue
		}
		wj, _ := json.Marshal(w)
		hj, _ := json.Marshal(h)
		if string(wj) != string(hj) {
			return false
		}
	}
	return true
}

// approvals posts garrison/approval on every open pull request of config:
// its agents must be valid, and it needs an approval by Approval. base is
// members.yaml on main; nil for garrison, which has none and whose changes
// root admins approve.
func (m *Members) approvals(ctx context.Context, ns string, base *Roster, roots []string) error {
	g := m.Gitea
	repo := "/repos/" + ns + "/" + ConfigRepo
	var prs []struct {
		Number int `json:"number"`
		User   struct {
			Login string `json:"login"`
		} `json:"user"`
		Head struct {
			Ref  string `json:"ref"`
			SHA  string `json:"sha"`
			Repo struct {
				FullName string `json:"full_name"`
			} `json:"repo"`
		} `json:"head"`
	}
	if _, err := g.do(ctx, "", http.MethodGet, repo+"/pulls?state=open&limit=50", nil, &prs); err != nil {
		return err
	}
	var pushed map[string][]string
	if len(prs) > 0 {
		var err error
		if pushed, err = g.Pushers(ctx, ns, ConfigRepo); err != nil {
			return err
		}
	}
	for _, pr := range prs {
		state, desc := "pending", ""
		var raw string
		head, before := Roster{}, Roster{}
		var rosterErr error
		if base != nil {
			before = *base
			if _, err := g.do(ctx, "", http.MethodGet, repo+"/raw/"+MembersFile+"?ref="+pr.Head.SHA, nil, &raw); err != nil {
				rosterErr = errors.New("members.yaml is missing")
			} else {
				head, rosterErr = ParseRoster([]byte(raw))
			}
		}
		list, problems, err := g.Agents(ctx, ns, pr.Head.SHA)
		if err != nil {
			return err
		}
		more, err := g.grantProblems(ctx, ns, list)
		if err != nil {
			return err
		}
		problems = append(problems, more...)
		more, err = g.modelProblems(ctx, ns, pr.Head.SHA, list)
		if err != nil {
			return err
		}
		problems = append(problems, more...)
		stacks, err := g.stackProblems(ctx, ns, pr.Head.SHA)
		if err != nil {
			return err
		}
		if ns == PlatformUnit && strings.HasPrefix(pr.Head.Ref, SkillBranch) {
			files, err := g.pullFiles(ctx, ns, ConfigRepo, pr.Number)
			if err != nil {
				return err
			}
			if why := skillOnly(files); why != "" {
				problems = append(problems, why)
			}
		}
		if rosterErr != nil {
			state, desc = "failure", rosterErr.Error()
		} else if len(problems) > 0 {
			state, desc = "failure", "agents: "+strings.Join(problems, "; ")
		} else if len(stacks) > 0 {
			state, desc = "failure", "stacks: "+strings.Join(stacks, "; ")
		} else {
			var reviews []struct {
				User struct {
					Login string `json:"login"`
				} `json:"user"`
				State     string `json:"state"`
				Stale     bool   `json:"stale"`
				Dismissed bool   `json:"dismissed"`
				// Official: Gitea counts it towards the rule's approvals,
				// as its author was in an approving team when they gave it.
				Official bool `json:"official"`
			}
			if _, err := g.do(ctx, "", http.MethodGet, fmt.Sprintf("%s/pulls/%d/reviews", repo, pr.Number), nil, &reviews); err != nil {
				return err
			}
			// An approval Gitea does not count would pass this check and
			// still leave the merge refused. It happens to an admin who
			// approves in their first visit to Gitea, which makes their
			// account a moment before the sync puts it in admins.
			var approvers, uncounted []string
			for _, r := range reviews {
				if r.State != "APPROVED" || r.Stale || r.Dismissed {
					continue
				}
				if r.Official {
					approvers = append(approvers, r.User.Login)
				} else if !slices.Contains(uncounted, r.User.Login) {
					uncounted = append(uncounted, r.User.Login)
				}
			}
			// Who wrote it: who opened it, and who pushed to its branch.
			authors := []string{pr.User.Login}
			if pr.Head.Repo.FullName == ns+"/"+ConfigRepo {
				for _, who := range pushed[pr.Head.Ref] {
					if !slices.Contains(authors, who) {
						authors = append(authors, who)
					}
				}
			}
			ok, why := Approval(before, head, authors, approvers, roots)
			if ns == PlatformUnit && !ok {
				// The only root admin has nobody to approve their own
				// change (Gitea lets nobody approve their own pull
				// request): it goes in, and merging is their review.
				if only, alone := SoleRoot(authors, people(roots, g.User)); alone {
					ok, why = true, only+" is the only root admin: merging it is the review"
				}
			}
			desc = why
			if ok {
				state = "success"
			} else if len(uncounted) > 0 {
				desc = "Gitea does not count the approval of " + strings.Join(uncounted, ", ") +
					", given before they were in this unit's team: approve again; " + why
			}
		}
		if len(desc) > 250 {
			desc = desc[:250]
		}
		key := pr.Head.SHA + " " + state + " " + desc
		if m.posted[pr.Head.SHA] == key {
			continue
		}
		if _, err := g.do(ctx, "", http.MethodPost, repo+"/statuses/"+pr.Head.SHA, map[string]string{
			"state": state, "context": ApprovalContext, "description": desc,
		}, nil); err != nil {
			return err
		}
		m.posted[pr.Head.SHA] = key
		log.Printf("members: %s/config#%d: %s: %s", ns, pr.Number, state, desc)
	}
	return nil
}

// Roster reads a unit's members.yaml on main, for the agent.
func (g *Gitea) Roster(ctx context.Context, as, ns string) (string, error) {
	return g.Read(ctx, as, ns, ConfigRepo, MembersFile)
}

// Run syncs now and then every interval, logging failures.
func (m *Members) Run(ctx context.Context, interval time.Duration) {
	for {
		if err := m.Sync(ctx); err != nil {
			log.Printf("members: %v", err)
		}
		if !hooks.Sleep(ctx, interval, m.Wake) {
			return
		}
	}
}
