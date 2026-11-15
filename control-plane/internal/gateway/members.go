package gateway

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"homecloud/control-plane/internal/agents"
	"homecloud/control-plane/internal/hooks"
)

// Members keeps each namespace's config repo: who may change the namespace,
// as members.yaml there says, and the checks on its agents. For system,
// which has no members.yaml, only root admins approve changes to config.
// Who may change a namespace, as members.yaml says:
//
//	admins: [alice]   # approve and merge changes, including to members.yaml
//	members: [bob]    # push to the namespace's repos
//
// Everyone signed in may read everything. The file is the truth: Gitea's teams
// "admins" and "members" are kept in line with it. It changes only through
// pull requests, which need the approval of an admin of the namespace or a
// root admin who neither wrote the pull request nor has their role changed by
// it. Gitea cannot check the last part, so this posts a required status,
// homecloud/approval, on every open pull request of config.
type Members struct {
	// Wake, when set, starts a sync before the interval is over.
	Wake  <-chan struct{}
	Gitea *Gitea

	posted  map[string]string // pull request head -> status last posted
	applied map[string]string // agent account -> the repos it was last given
	current map[string]string // pull request -> its head and base when last brought up to date
}

const (
	// RootTeam holds the platform's root admins in every namespace, kept from
	// Gitea's site admins: Gitea counts approvals and merges only from people
	// with write access to the repo, which root admins otherwise lack.
	RootTeam        = "root-admins"
	ConfigRepo      = "config"
	MembersFile     = "members.yaml"
	ApprovalContext = "homecloud/approval"
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
		return r, errors.New("members.yaml: a namespace needs at least one admin")
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
// admin of the namespace (by the roster on main) or a root admin approves it
// who neither wrote it nor has their role changed by it.
func Approval(base, head Roster, author string, approvers, rootAdmins []string) (bool, string) {
	changed := Changed(base, head)
	var refused []string
	for _, a := range approvers {
		switch {
		case a == author:
			refused = append(refused, a+" wrote it")
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

var allUnits = []string{"repo.code", "repo.issues", "repo.pulls", "repo.releases", "repo.wiki", "repo.projects", "repo.packages", "repo.actions"}

func unitsMap(access string) map[string]string {
	m := map[string]string{}
	for _, u := range allUnits {
		m[u] = access
	}
	return m
}

// Sync sets up and applies every namespace's members.
func (m *Members) Sync(ctx context.Context) error {
	if m.posted == nil {
		m.posted = map[string]string{}
	}
	namespaces, err := m.Gitea.Namespaces(ctx, "")
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
	for _, ns := range namespaces {
		present[ns.Name] = true
		if ns.Name == SystemNamespace {
			if err := m.system(ctx, roots); err != nil {
				errs = append(errs, fmt.Errorf("config of system: %w", err))
			}
		} else if err := m.namespace(ctx, ns.Name, roots); err != nil {
			errs = append(errs, fmt.Errorf("members of %s: %w", ns.Name, err))
		}
		if err := m.accounts(ctx, ns.Name, users); err != nil {
			errs = append(errs, fmt.Errorf("agent accounts of %s: %w", ns.Name, err))
		}
		if err := m.keepCurrent(ctx, ns.Name); err != nil {
			errs = append(errs, fmt.Errorf("changes of %s: %w", ns.Name, err))
		}
	}
	// The agents of a namespace that is gone: their accounts are locked, so
	// a namespace made again under its name starts without them.
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
		log.Printf("members: locked the account %s: its namespace %s is gone", u.Login, ns)
	}
	return errors.Join(errs...)
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

// accounts keeps the Gitea accounts of a namespace's agents that have
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
	}
	return nil
}

// collaborations gives account write access to the repos in granted and
// takes it away from every other repo of the namespace. It is applied again
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

// teams lists a namespace's teams, making the ones in want (name: access)
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

func (m *Members) namespace(ctx context.Context, ns string, roots []string) error {
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
		files := agents.OperatorFiles(ToolNames())
		files[MembersFile] = "# Who may change the namespace " + ns + ". Everyone signed in may read everything.\n" +
			"# admins approve and merge changes, including to this file; members push to its repos.\n" +
			"# Change it by pull request: it needs the approval of an admin who did not write it\n" +
			"# and whose role it does not change.\n" + string(data)
		if err := m.createConfig(ctx, ns, "Who may change this namespace, and its agents", "First admins: the namespace's creators; the operator agent", files); err != nil {
			return err
		}
		log.Printf("members: %s/config made, admins %v", ns, owners)
	} else if err != nil {
		return err
	}
	if err := m.protect(ctx, ns, []string{"admins", RootTeam}); err != nil {
		return err
	}

	// Apply members.yaml on main to the teams.
	var raw string
	if _, err := g.do(ctx, "", http.MethodGet, "/repos/"+ns+"/"+ConfigRepo+"/raw/"+MembersFile+"?ref=main", nil, &raw); err != nil {
		return err
	}
	roster, err := ParseRoster([]byte(raw))
	if err != nil {
		return err // keep the teams as they are
	}
	for name, want := range map[string][]string{"admins": roster.Admins, "members": roster.Members, RootTeam: roots} {
		if err := m.syncTeam(ctx, ns, name, teams[name], want); err != nil {
			return err
		}
	}
	return m.approvals(ctx, ns, &roster, roots)
}

// system keeps system/config, which holds the system namespace's agents.
// It has no members.yaml: root admins approve and merge its changes.
func (m *Members) system(ctx context.Context, roots []string) error {
	ns := SystemNamespace
	teams, err := m.teams(ctx, ns, map[string]string{RootTeam: "admin"})
	if err != nil {
		return err
	}
	if status, err := m.Gitea.do(ctx, "", http.MethodGet, "/repos/"+ns+"/"+ConfigRepo, nil, nil); status == http.StatusNotFound {
		if err := m.createConfig(ctx, ns, "The agents of the system namespace; root admins approve changes", "The operator agent", agents.OperatorFiles(ToolNames())); err != nil {
			return err
		}
		log.Printf("members: %s/config made", ns)
	} else if err != nil {
		return err
	}
	if err := m.protect(ctx, ns, []string{RootTeam}); err != nil {
		return err
	}
	if err := m.syncTeam(ctx, ns, RootTeam, teams[RootTeam], roots); err != nil {
		return err
	}
	return m.approvals(ctx, ns, nil, roots)
}

// createConfig makes a namespace's config repo with files, in one commit.
func (m *Members) createConfig(ctx context.Context, ns, description, message string, files map[string]string) error {
	g := m.Gitea
	if _, err := g.do(ctx, "", http.MethodPost, "/orgs/"+ns+"/repos", map[string]any{
		"name": ConfigRepo, "description": description,
		"auto_init": true, "default_branch": "main", "private": false,
	}, nil); err != nil {
		return err
	}
	var ops []map[string]string
	for _, path := range sortedKeys(files) {
		ops = append(ops, map[string]string{"operation": "create", "path": path, "content": b64(files[path])})
	}
	_, err := g.do(ctx, "", http.MethodPost, "/repos/"+ns+"/"+ConfigRepo+"/contents", map[string]any{
		"branch": "main", "message": message, "files": ops,
	}, nil)
	return err
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
// mergers are the teams that approve and merge.
func (m *Members) protect(ctx context.Context, ns string, mergers []string) error {
	rule := map[string]any{
		"rule_name":                    "main",
		"enable_push":                  false,
		"enable_merge_whitelist":       true,
		"merge_whitelist_teams":        mergers,
		"merge_whitelist_usernames":    []string{},
		"enable_approvals_whitelist":   true,
		"approvals_whitelist_teams":    mergers,
		"approvals_whitelist_username": []string{},
		"required_approvals":           1,
		"enable_status_check":          true,
		"status_check_contexts":        []string{ApprovalContext},
		"block_on_rejected_reviews":    true,
		"dismiss_stale_approvals":      true,
		"block_admin_merge_override":   true,
	}
	path := "/repos/" + ns + "/" + ConfigRepo + "/branch_protections"
	var have map[string]any
	if status, err := m.Gitea.do(ctx, "", http.MethodGet, path+"/main", nil, &have); status == http.StatusNotFound {
		_, err := m.Gitea.do(ctx, "", http.MethodPost, path, rule, nil)
		return err
	} else if err != nil {
		return err
	}
	// Written only when it differs: every change of a rule makes Gitea check
	// the branch's open pull requests again, and refuse merges meanwhile.
	if sameRule(rule, have) {
		return nil
	}
	delete(rule, "rule_name")
	_, err := m.Gitea.do(ctx, "", http.MethodPatch, path+"/main", rule, nil)
	return err
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

// approvals posts homecloud/approval on every open pull request of config:
// its agents must be valid, and it needs an approval by Approval. base is
// members.yaml on main; nil for system, which has none and whose changes
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
			SHA string `json:"sha"`
		} `json:"head"`
	}
	if _, err := g.do(ctx, "", http.MethodGet, repo+"/pulls?state=open&limit=50", nil, &prs); err != nil {
		return err
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
		stacks, err := g.stackProblems(ctx, ns, pr.Head.SHA)
		if err != nil {
			return err
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
			}
			if _, err := g.do(ctx, "", http.MethodGet, fmt.Sprintf("%s/pulls/%d/reviews", repo, pr.Number), nil, &reviews); err != nil {
				return err
			}
			var approvers []string
			for _, r := range reviews {
				if r.State == "APPROVED" && !r.Stale && !r.Dismissed {
					approvers = append(approvers, r.User.Login)
				}
			}
			ok, why := Approval(before, head, pr.User.Login, approvers, roots)
			desc = why
			if ok {
				state = "success"
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

// Roster reads a namespace's members.yaml on main, for the agent.
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
