package gateway

// Agents' own accounts and grants. By default an agent acts as the person
// talking to it. A namespace can give an agent rights of its own in
// agent.yaml (grants), used only for requests of the groups each grant
// names, with an account of its own in Gitea. See docs/agents.md.

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	"homecloud/control-plane/internal/agents"
)

// AgentAccount is the Gitea account of an agent: agent-<namespace>-<name>,
// or a hash of it where that is longer than Gitea allows.
func AgentAccount(a *agents.Agent) string {
	name := "agent-" + a.Namespace + "-" + a.Name
	if len(name) <= 40 {
		return name
	}
	sum := sha256.Sum256([]byte(a.ID()))
	return "agent-" + hex.EncodeToString(sum[:8])
}

// AccountPrefix starts the full name of every agent account, which is how
// the gateway knows them.
const AccountPrefix = "Agent "

// ServiceKey is what the agent service shows the gateway to say who a run
// is for and what started it, and to run without a person. Both derive it
// from the admin password they hold, so no further secret is needed.
func ServiceKey(adminPassword string) string {
	m := hmac.New(sha256.New, []byte(adminPassword))
	m.Write([]byte("homecloud agent service v1"))
	return hex.EncodeToString(m.Sum(nil))
}

// Role is a person's role in a namespace, from its members.yaml on main:
// admin, member or "". In system, which has none, root admins are its
// admins.
func (g *Gitea) Role(ctx context.Context, ns, person string) (string, error) {
	if err := validOrg(ns); err != nil {
		return "", err
	}
	if ns == SystemNamespace {
		var u struct {
			IsAdmin bool `json:"is_admin"`
		}
		status, err := g.do(ctx, "", http.MethodGet, "/users/"+person, nil, &u)
		if status == http.StatusNotFound {
			return "", nil
		}
		if err != nil {
			return "", err
		}
		if u.IsAdmin {
			return "admin", nil
		}
		return "", nil
	}
	var raw string
	status, err := g.do(ctx, "", http.MethodGet, "/repos/"+ns+"/"+ConfigRepo+"/raw/"+MembersFile+"?ref=main", nil, &raw)
	if status == http.StatusNotFound {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	r, err := ParseRoster([]byte(raw))
	if err != nil {
		return "", err
	}
	return r.Role(person), nil
}

// role is the role grants are checked against: that of the person asking,
// or of a routine's owner in a run without a person. Root admins are
// admins everywhere.
func (t *Tools) role(ctx context.Context, p Person, ns string) (string, error) {
	who := p.Name
	if p.Unattended {
		who = p.Owner
	} else if p.Admin {
		return "admin", nil
	}
	return t.Gitea.Role(ctx, ns, who)
}

// asker names who a change was for, in pull requests and logs.
func (p Person) asker() string {
	if p.Unattended {
		return p.Owner
	}
	return p.Name
}

// CanUse tells whether p may talk to agent: anyone, unless it is private
// (AgentPrivate); then the people of its namespace and root admins.
func (g *Gitea) CanUse(ctx context.Context, a *agents.Agent, p Person) (bool, error) {
	if a == nil || p.Admin || p.Unattended {
		return true, nil
	}
	private, err := g.AgentPrivate(ctx, a)
	if err != nil || !private {
		return !private, err
	}
	role, err := g.Role(ctx, a.Namespace, p.Name)
	return role != "", err
}

// refused is a refusal the agent should read as it is, with what would be
// needed.
type refused struct{ msg string }

func (r refused) Error() string { return "not allowed: " + r.msg }

// proposer decides as whom a proposal to org/repo is made: the person, if
// they may write to it; else the agent's account, if one of its grants
// lets it propose there for them.
func (t *Tools) proposer(ctx context.Context, p Person, agent *agents.Agent, org, repo string) (as string, viaAgent bool, err error) {
	if !p.Unattended {
		err := t.Gitea.CanWrite(ctx, p.Name, org, repo)
		if err == nil {
			return p.Name, false, nil
		}
		if !errors.Is(err, ErrForbidden) {
			return "", false, err
		}
	}
	if agent != nil && agent.Namespace == org {
		if err := t.mayRead(ctx, p, org, repo); err != nil {
			return "", false, err
		}
		role, err := t.role(ctx, p, org)
		if err != nil {
			return "", false, err
		}
		if _, ok := agent.GrantFor(repo, "", agents.LevelPropose, role, p.Unattended); ok {
			return AgentAccount(agent), true, nil
		}
	}
	if p.Unattended {
		return "", false, refused{fmt.Sprintf("this run has no person, and %s has no grant with unattended: true to propose in %s/%s", agentID(agent), org, repo)}
	}
	return "", false, refused{fmt.Sprintf("%s may not write to %s/%s, and %s has no grant that lets it propose there for them. Say so; an admin of %s can add a grant in its agent.yaml", p.Name, org, repo, agentID(agent), org)}
}

// newBranch is the branch of a new proposal: change/<person>-<ms> when made
// as the person, agent/<agent>/<person>-<ms> when made by the agent's
// account.
func newBranch(p Person, agent *agents.Agent, viaAgent bool) string {
	ms := time.Now().UnixNano() / 1e6
	if viaAgent {
		return fmt.Sprintf("%s%s-%d", agent.Branch(), p.asker(), ms)
	}
	return fmt.Sprintf("%s%s-%d", ChangeBranch, p.Name, ms)
}

// askedBy is the line the gateway writes into an agent's pull requests,
// and into a comment for each later addition, naming whom it was for.
func askedBy(p Person, agent *agents.Agent, role string) string {
	who := p.asker()
	if role == "" && !p.Unattended {
		who += " (not in " + agent.Namespace + ")"
	}
	via := agent.ID()
	if p.Unattended {
		via += ", " + p.StartedBy + ", with nobody watching"
	} else if p.StartedBy != "" && p.StartedBy != "person" {
		via += ", " + p.StartedBy
	}
	return fmt.Sprintf("Asked by %s through %s.", who, via)
}

var askedByLine = regexp.MustCompile(`(?m)^(?:Changes added, a|A)sked by ([A-Za-z0-9._-]+)`)

// mergeChange merges a pull request with the agent's account, if a grant
// covers its base branch for the person and its checks are green.
func (t *Tools) mergeChange(ctx context.Context, p Person, agent *agents.Agent, org, repo string, number int) (string, error) {
	if agent == nil {
		return "", refused{"only agents with a grant merge"}
	}
	if err := validOrg(org); err != nil {
		return "", err
	}
	if err := validRepo(repo); err != nil {
		return "", err
	}
	if org != agent.Namespace {
		return "", refused{fmt.Sprintf("%s has grants only in %s", agent.ID(), agent.Namespace)}
	}
	if number <= 0 {
		return "", fmt.Errorf("%w: the pull request's number is missing", ErrInvalid)
	}
	if err := t.mayRead(ctx, p, org, repo); err != nil {
		return "", err
	}
	g := t.Gitea
	path := fmt.Sprintf("/repos/%s/%s/pulls/%d", org, repo, number)
	var pr struct {
		State   string `json:"state"`
		Merged  bool   `json:"merged"`
		Body    string `json:"body"`
		HTMLURL string `json:"html_url"`
		User    struct {
			Login string `json:"login"`
		} `json:"user"`
		Base struct {
			Ref string `json:"ref"`
		} `json:"base"`
		Head struct {
			Ref  string `json:"ref"`
			SHA  string `json:"sha"`
			Repo struct {
				FullName string `json:"full_name"`
			} `json:"repo"`
		} `json:"head"`
	}
	if status, err := g.do(ctx, "", http.MethodGet, path, nil, &pr); status == http.StatusNotFound {
		return "", fmt.Errorf("%w: there is no pull request %s/%s#%d", ErrInvalid, org, repo, number)
	} else if err != nil {
		return "", err
	}
	switch {
	case pr.Merged:
		return fmt.Sprintf("%s/%s#%d is merged already.", org, repo, number), nil
	case pr.State != "open":
		return "", refused{fmt.Sprintf("%s/%s#%d is closed", org, repo, number)}
	case pr.Head.Repo.FullName != org+"/"+repo:
		return "", refused{"it comes from another repo"}
	case !strings.HasPrefix(pr.Head.Ref, ChangeBranch) && !strings.HasPrefix(pr.Head.Ref, agent.Branch()):
		return "", refused{fmt.Sprintf("agents merge only proposals (branches %s... and %s...), not %s", ChangeBranch, agent.Branch(), pr.Head.Ref)}
	}
	role, err := t.role(ctx, p, org)
	if err != nil {
		return "", err
	}
	grant, ok := agent.GrantFor(repo, pr.Base.Ref, agents.LevelMerge, role, p.Unattended)
	if !ok {
		return "", refused{fmt.Sprintf("%s has no grant to merge into %s of %s/%s for %s (role: %s%s). Say so: a person allowed to merges it in Gitea, or an admin of %s adds a grant in its agent.yaml",
			agent.ID(), pr.Base.Ref, org, repo, p.asker(), roleName(role), map[bool]string{true: ", with nobody watching"}[p.Unattended], org)}
	}
	if agents.Rank(grant.Level) < agents.Rank(agents.LevelAutonomous) && slices.Contains(agents.Deploying, pr.Base.Ref) {
		return "", refused{"a merge into " + pr.Base.Ref + " deploys, which needs autonomous"}
	}
	// Green checks.
	if len(grant.Checks) > 0 {
		var st struct {
			Statuses []struct {
				Context string `json:"context"`
				Status  string `json:"status"`
			} `json:"statuses"`
		}
		if _, err := g.do(ctx, "", http.MethodGet, fmt.Sprintf("/repos/%s/%s/commits/%s/status", org, repo, pr.Head.SHA), nil, &st); err != nil {
			return "", err
		}
		for _, want := range grant.Checks {
			state := "missing"
			for _, s := range st.Statuses {
				if s.Context == want {
					state = s.Status
				}
			}
			if state != "success" {
				return "", refused{fmt.Sprintf("the check %s is %s on %.12s; wait until it passes, then ask again", want, state, pr.Head.SHA)}
			}
		}
	}
	// Whose work it is: the pull request's author, if a person, and everyone
	// named by an "Asked by" line. Someone else's work needs an approval by
	// someone who did not ask for it, so nobody puts another's unreviewed
	// work live through an agent.
	askers := []string{}
	if !strings.HasPrefix(pr.User.Login, "agent-") {
		askers = append(askers, pr.User.Login)
	}
	texts := []string{pr.Body}
	var comments []struct {
		Body string `json:"body"`
	}
	if _, err := g.do(ctx, "", http.MethodGet, fmt.Sprintf("/repos/%s/%s/issues/%d/comments", org, repo, number), nil, &comments); err != nil {
		return "", err
	}
	for _, c := range comments {
		texts = append(texts, c.Body)
	}
	for _, s := range texts {
		for _, m := range askedByLine.FindAllStringSubmatch(s, -1) {
			if !slices.Contains(askers, m[1]) {
				askers = append(askers, m[1])
			}
		}
	}
	others := slices.DeleteFunc(slices.Clone(askers), func(n string) bool { return n == p.asker() })
	if len(others) > 0 {
		var reviews []struct {
			User struct {
				Login string `json:"login"`
			} `json:"user"`
			State     string `json:"state"`
			Stale     bool   `json:"stale"`
			Dismissed bool   `json:"dismissed"`
		}
		if _, err := g.do(ctx, "", http.MethodGet, path+"/reviews", nil, &reviews); err != nil {
			return "", err
		}
		approved := slices.ContainsFunc(reviews, func(r struct {
			User struct {
				Login string `json:"login"`
			} `json:"user"`
			State     string `json:"state"`
			Stale     bool   `json:"stale"`
			Dismissed bool   `json:"dismissed"`
		}) bool {
			return r.State == "APPROVED" && !r.Stale && !r.Dismissed && !slices.Contains(askers, r.User.Login)
		})
		if !approved {
			return "", refused{fmt.Sprintf("this change holds work asked for by %s; it needs an approval in Gitea by someone who did not ask for it before an agent merges it", strings.Join(others, ", "))}
		}
	}
	account := AgentAccount(agent)
	msg := fmt.Sprintf("Merge %s/%s#%d\n\n%s", org, repo, number, askedBy(p, agent, role))
	// Gitea checks whether it merges cleanly after each push; it says so
	// itself, rather than this trusting a check still running.
	if status, err := g.do(ctx, account, http.MethodPost, path+"/merge", map[string]any{
		"Do": "merge", "MergeTitleField": fmt.Sprintf("Merge #%d", number), "MergeMessageField": msg,
		"delete_branch_after_merge": true, "head_commit_id": pr.Head.SHA,
	}, nil); err != nil {
		switch {
		case errors.Is(err, ErrForbidden):
			return "", refused{fmt.Sprintf("Gitea refused the merge by %s; its account may not be set up yet (the gateway sets it up within a minute of the grant)", account)}
		case status == http.StatusMethodNotAllowed || status == http.StatusConflict:
			return "", refused{fmt.Sprintf("Gitea cannot merge %s/%s#%d now: it conflicts with %s, or Gitea is still checking it after the last push (then try again in a minute): %v", org, repo, number, pr.Base.Ref, err)}
		}
		return "", err
	}
	out := fmt.Sprintf("Merged %s/%s#%d into %s with the account %s. %s", org, repo, number, pr.Base.Ref, account, askedBy(p, agent, role))
	if grant.Level == agents.LevelAutonomous {
		out += fmt.Sprintf(" The deployer deploys %s once its pipeline has built the image, usually within minutes.", pr.Base.Ref)
	}
	return out + "\n" + pr.HTMLURL, nil
}

func roleName(r string) string {
	if r == "" {
		return "not in the namespace"
	}
	return r
}

// proposePatch proposes a patch as the person, or with the agent's account
// where only a grant allows it.
func (t *Tools) proposePatch(ctx context.Context, p Person, agent *agents.Agent, a args) (string, error) {
	if err := validOrg(a.Namespace); err != nil {
		return "", err
	}
	if err := validRepo(a.Repo); err != nil {
		return "", err
	}
	as, viaAgent, err := t.proposer(ctx, p, agent, a.Namespace, a.Repo)
	if err != nil {
		return "", err
	}
	pp := Proposal{
		Org: a.Namespace, Repo: a.Repo, Base: a.Base, Branch: a.Branch,
		Fresh: newBranch(p, agent, viaAgent), Prefixes: []string{ChangeBranch},
		Patch: a.Patch, Title: a.Title, Description: a.Description,
	}
	if agent != nil {
		pp.Prefixes = append(pp.Prefixes, agent.Branch())
		role := "member"
		if viaAgent {
			if role, err = t.role(ctx, p, a.Namespace); err != nil {
				return "", err
			}
		}
		pp.Note = askedBy(p, agent, role)
	}
	c, branch, err := t.Gitea.ProposePatch(ctx, as, pp)
	if err != nil {
		return "", err
	}
	by := ""
	if viaAgent {
		by = fmt.Sprintf(" It is by the agent's account %s, for %s.", as, p.asker())
	}
	next := "Nothing is changed yet: a person allowed to merge must review and merge it in Gitea."
	if agent != nil && slices.ContainsFunc(agent.Grants, func(g agents.Grant) bool {
		return g.Repo == a.Repo && agents.Rank(g.Level) >= agents.Rank(agents.LevelMerge)
	}) && slices.Contains(agent.Tools, "merge_change") {
		next = "Nothing is changed yet. If the person wants it live and one of your grants covers it, merge it with merge_change once its checks pass; otherwise a person merges it in Gitea."
	}
	return fmt.Sprintf("Change %s/%s#%d now holds the patch, on branch %s.%s\nBranch: %s\n%s\nEnd your answer with this link, exactly as it is, so the person can open it:\n%s", c.Namespace, c.Repo, c.Number, branch, by, branch, next, c.URL), nil
}

// BranchHead is the commit a branch points at, as the admin; "" if there
// is no such branch.
func (g *Gitea) BranchHead(ctx context.Context, org, repo, branch string) (string, error) {
	if err := validOrg(org); err != nil {
		return "", err
	}
	if err := validRepo(repo); err != nil {
		return "", err
	}
	var b struct {
		Commit struct {
			ID string `json:"id"`
		} `json:"commit"`
	}
	status, err := g.do(ctx, "", http.MethodGet, fmt.Sprintf("/repos/%s/%s/branches/%s", org, repo, escapePath(branch)), nil, &b)
	if status == http.StatusNotFound {
		return "", nil
	}
	return b.Commit.ID, err
}
