package controlplane

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Members keeps who may change each namespace other than system, as its repo
// config says in members.yaml:
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
	Gitea *Gitea

	posted map[string]string // pull request head -> status last posted
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
	roots, err := m.rootAdmins(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, ns := range namespaces {
		if ns.Name == SystemNamespace {
			continue
		}
		if err := m.namespace(ctx, ns.Name, roots); err != nil {
			errs = append(errs, fmt.Errorf("members of %s: %w", ns.Name, err))
		}
	}
	return errors.Join(errs...)
}

func (m *Members) rootAdmins(ctx context.Context) ([]string, error) {
	var out []string
	for page := 1; page < 20; page++ {
		var users []struct {
			Login   string `json:"login"`
			IsAdmin bool   `json:"is_admin"`
		}
		if _, err := m.Gitea.do(ctx, "", http.MethodGet, fmt.Sprintf("/admin/users?limit=50&page=%d", page), nil, &users); err != nil {
			return nil, err
		}
		for _, u := range users {
			if u.IsAdmin {
				out = append(out, u.Login)
			}
		}
		if len(users) < 50 {
			break
		}
	}
	return out, nil
}

type team struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

func (m *Members) teams(ctx context.Context, ns string) (map[string]int64, error) {
	var list []team
	if _, err := m.Gitea.do(ctx, "", http.MethodGet, "/orgs/"+ns+"/teams", nil, &list); err != nil {
		return nil, err
	}
	out := map[string]int64{}
	for _, t := range list {
		out[t.Name] = t.ID
	}
	for name, access := range map[string]string{"admins": "admin", "members": "write", RootTeam: "admin"} {
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
		return "The platform's root admins, kept by the control plane."
	}
	return "Kept from config/members.yaml; change it there."
}

func (m *Members) teamMembers(ctx context.Context, id int64) ([]string, error) {
	var users []struct {
		Login string `json:"login"`
	}
	if _, err := m.Gitea.do(ctx, "", http.MethodGet, fmt.Sprintf("/teams/%d/members?limit=100", id), nil, &users); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(users))
	for _, u := range users {
		out = append(out, u.Login)
	}
	return out, nil
}

func (m *Members) namespace(ctx context.Context, ns string, roots []string) error {
	g := m.Gitea
	teams, err := m.teams(ctx, ns)
	if err != nil {
		return err
	}

	// config: created with the organisation's owners as its first admins.
	if status, err := g.do(ctx, "", http.MethodGet, "/repos/"+ns+"/"+ConfigRepo, nil, nil); status == http.StatusNotFound {
		owners, err := m.teamMembers(ctx, teams["Owners"])
		if err != nil {
			return err
		}
		owners = slices.DeleteFunc(owners, func(n string) bool { return n == BuilderName(ns) })
		if len(owners) == 0 {
			return errors.New("no owner to make the first admin")
		}
		if _, err := g.do(ctx, "", http.MethodPost, "/orgs/"+ns+"/repos", map[string]any{
			"name": ConfigRepo, "description": "Who may change this namespace: members.yaml",
			"auto_init": true, "default_branch": "main", "private": false,
		}, nil); err != nil {
			return err
		}
		data, _ := yaml.Marshal(Roster{Admins: owners, Members: []string{}})
		content := "# Who may change the namespace " + ns + ". Everyone signed in may read everything.\n" +
			"# admins approve and merge changes, including to this file; members push to its repos.\n" +
			"# Change it by pull request: it needs the approval of an admin who did not write it\n" +
			"# and whose role it does not change.\n" + string(data)
		if _, err := g.do(ctx, "", http.MethodPost, "/repos/"+ns+"/"+ConfigRepo+"/contents/"+MembersFile, map[string]any{
			"branch": "main", "message": "First admins: the namespace's creators", "content": b64(content),
		}, nil); err != nil {
			return err
		}
		log.Printf("members: %s/config made, admins %v", ns, owners)
	} else if err != nil {
		return err
	}
	if err := m.protect(ctx, ns); err != nil {
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
		have, err := m.teamMembers(ctx, teams[name])
		if err != nil {
			return err
		}
		for _, u := range want {
			if slices.Contains(have, u) {
				continue
			}
			if status, err := g.do(ctx, "", http.MethodPut, fmt.Sprintf("/teams/%d/members/%s", teams[name], url.PathEscape(u)), nil, nil); err != nil {
				if status == http.StatusNotFound {
					continue // no Gitea account yet: added after their first sign-in
				}
				return err
			}
			log.Printf("members: %s is now in %s/%s", u, ns, name)
		}
		for _, u := range have {
			if slices.Contains(want, u) {
				continue
			}
			if _, err := g.do(ctx, "", http.MethodDelete, fmt.Sprintf("/teams/%d/members/%s", teams[name], url.PathEscape(u)), nil, nil); err != nil {
				return err
			}
			log.Printf("members: %s is no longer in %s/%s", u, ns, name)
		}
	}
	return m.approvals(ctx, ns, roster, roots)
}

// protect keeps config's main branch changing only by approved pull requests.
func (m *Members) protect(ctx context.Context, ns string) error {
	rule := map[string]any{
		"rule_name":                    "main",
		"enable_push":                  false,
		"enable_merge_whitelist":       true,
		"merge_whitelist_teams":        []string{"admins", RootTeam},
		"merge_whitelist_usernames":    []string{},
		"enable_approvals_whitelist":   true,
		"approvals_whitelist_teams":    []string{"admins", RootTeam},
		"approvals_whitelist_username": []string{},
		"required_approvals":           1,
		"enable_status_check":          true,
		"status_check_contexts":        []string{ApprovalContext},
		"block_on_rejected_reviews":    true,
		"dismiss_stale_approvals":      true,
		"block_admin_merge_override":   true,
	}
	path := "/repos/" + ns + "/" + ConfigRepo + "/branch_protections"
	if status, err := m.Gitea.do(ctx, "", http.MethodGet, path+"/main", nil, nil); status == http.StatusNotFound {
		_, err := m.Gitea.do(ctx, "", http.MethodPost, path, rule, nil)
		return err
	} else if err != nil {
		return err
	}
	delete(rule, "rule_name")
	_, err := m.Gitea.do(ctx, "", http.MethodPatch, path+"/main", rule, nil)
	return err
}

// approvals posts homecloud/approval on every open pull request of config.
func (m *Members) approvals(ctx context.Context, ns string, base Roster, roots []string) error {
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
		if _, err := g.do(ctx, "", http.MethodGet, repo+"/raw/"+MembersFile+"?ref="+pr.Head.SHA, nil, &raw); err != nil {
			state, desc = "failure", "members.yaml is missing"
		} else if head, err := ParseRoster([]byte(raw)); err != nil {
			state, desc = "failure", err.Error()
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
			ok, why := Approval(base, head, pr.User.Login, approvers, roots)
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
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}
