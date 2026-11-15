package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"garrison/control-plane/internal/agents"
)

// grantsGitea is homepage/todo: alice is an admin, bob a member, carol is
// not in homepage; the agent's account writes once it is a collaborator.
type grantsGitea struct {
	mu       sync.Mutex
	writers  map[string]bool
	prs      map[int]map[string]any
	comments map[int][]string
	reviews  map[int][]map[string]any
	status   string // of the check ci
	merged   []string
	patched  []string         // as whom
	feed     []map[string]any // the repo's activity, newest first
}

func newGrantsGitea() *grantsGitea {
	return &grantsGitea{
		writers: map[string]bool{"alice": true, "bob": true, "agent-homepage-dev": true},
		prs:     map[int]map[string]any{}, comments: map[int][]string{}, reviews: map[int][]map[string]any{},
		status: "success",
	}
}

func (f *grantsGitea) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	as := r.Header.Get("Sudo")
	p := strings.TrimPrefix(r.URL.Path, "/api/v1")
	var n int
	switch {
	case p == "/repos/homepage/config/raw/members.yaml":
		io.WriteString(w, "admins: [alice]\nmembers: [bob]\n")
	case p == "/repos/homepage/secret":
		// Private: hidden from carol, who is not in homepage.
		if as == "carol" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprintf(w, `{"private":true,"permissions":{"push":%v}}`, f.writers[as])
	case p == "/repos/homepage/todo" && r.Method == http.MethodGet:
		fmt.Fprintf(w, `{"permissions":{"push":%v}}`, f.writers[as])
	case p == "/repos/homepage/todo/diffpatch":
		if !f.writers[as] {
			http.Error(w, `{"message":"forbidden"}`, http.StatusForbidden)
			return
		}
		f.patched = append(f.patched, as)
		io.WriteString(w, "{}")
	case p == "/repos/homepage/todo/pulls" && r.Method == http.MethodPost:
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		n = len(f.prs) + 1
		f.prs[n] = map[string]any{
			"number": n, "state": "open", "mergeable": true, "body": body["body"], "html_url": fmt.Sprintf("https://git/homepage/todo/pulls/%d", n),
			"user": map[string]any{"login": as}, "base": map[string]any{"ref": body["base"]},
			"head": map[string]any{"ref": body["head"], "sha": fmt.Sprintf("sha%d", n), "repo": map[string]any{"full_name": "homepage/todo"}},
		}
		json.NewEncoder(w).Encode(f.prs[n])
	case p == "/repos/homepage/todo/pulls":
		// Like Gitea: ?state=open leaves out the merged and closed ones.
		out := []any{}
		for _, pr := range f.prs {
			if s := r.URL.Query().Get("state"); s == "" || s == "all" || s == pr["state"] {
				out = append(out, pr)
			}
		}
		json.NewEncoder(w).Encode(out)
	case sscan(p, "/repos/homepage/todo/pulls/%d/merge", &n):
		f.merged = append(f.merged, fmt.Sprintf("%d by %s", n, as))
		f.prs[n]["merged"], f.prs[n]["state"] = true, "closed"
		io.WriteString(w, "{}")
	case sscan(p, "/repos/homepage/todo/pulls/%d/reviews", &n):
		json.NewEncoder(w).Encode(append([]map[string]any{}, f.reviews[n]...))
	case sscan(p, "/repos/homepage/todo/pulls/%d", &n):
		json.NewEncoder(w).Encode(f.prs[n])
	case sscan(p, "/repos/homepage/todo/issues/%d/comments", &n):
		if r.Method == http.MethodPost {
			var body map[string]string
			json.NewDecoder(r.Body).Decode(&body)
			f.comments[n] = append(f.comments[n], body["body"])
			io.WriteString(w, "{}")
			return
		}
		out := []map[string]string{}
		for _, c := range f.comments[n] {
			out = append(out, map[string]string{"body": c})
		}
		json.NewEncoder(w).Encode(out)
	case p == "/repos/homepage/todo/activities/feeds":
		out := []map[string]any{}
		if r.URL.Query().Get("page") == "1" {
			out = append(out, f.feed...)
		}
		json.NewEncoder(w).Encode(out)
	case strings.HasPrefix(p, "/repos/homepage/todo/commits/"):
		fmt.Fprintf(w, `{"statuses":[{"context":"ci","status":%q}]}`, f.status)
	default:
		http.NotFound(w, r)
	}
}

func sscan(p, format string, n *int) bool {
	var rest string
	_, err := fmt.Sscanf(p+" ", format+"%s", n, &rest)
	if err == nil {
		return false // more after it
	}
	_, err = fmt.Sscanf(p, format, n)
	return err == nil && fmt.Sprintf(format, *n) == p
}

func TestGrants(t *testing.T) {
	f := newGrantsGitea()
	srv := httptest.NewServer(f)
	defer srv.Close()
	tools := &Tools{Gitea: &Gitea{URL: srv.URL, User: "admin", Password: "pw"}}
	dev := &agents.Agent{Namespace: "homepage", Name: "dev", Tools: []string{"propose_patch", "merge_change"}, Grants: []agents.Grant{
		{Repo: "todo", Level: agents.LevelPropose, For: agents.ForEveryone},
		{Repo: "todo", Branch: "dev", Level: agents.LevelAutonomous, For: agents.ForMembers},
		{Repo: "todo", Branch: "main", Level: agents.LevelAutonomous, For: agents.ForAdmins, Checks: []string{"ci"}},
	}}
	ctx := context.Background()
	alice, bob, carol := Person{Name: "alice", StartedBy: "person"}, Person{Name: "bob", StartedBy: "person"}, Person{Name: "carol", StartedBy: "person"}
	propose := func(p Person, a *agents.Agent, base, branch string) (string, error) {
		raw, _ := json.Marshal(map[string]any{"namespace": "homepage", "repo": "todo", "patch": "diff --git a/x b/x\n", "title": "T", "base": base, "branch": branch})
		return tools.Call(ctx, p, a, "propose_patch", raw)
	}
	merge := func(p Person, n int) (string, error) {
		raw, _ := json.Marshal(map[string]any{"namespace": "homepage", "repo": "todo", "number": n})
		return tools.Call(ctx, p, dev, "merge_change", raw)
	}

	// bob may write himself: his proposal is his.
	if out, err := propose(bob, dev, "dev", ""); err != nil || !strings.Contains(out, "Branch: change/bob-") {
		t.Fatalf("bob: %q %v", out, err)
	}
	// carol may not, but dev may propose for everyone: by its account.
	out, err := propose(carol, dev, "dev", "")
	if err != nil || !strings.Contains(out, "Branch: agent/dev/carol-") || !strings.Contains(out, "agent-homepage-dev") {
		t.Fatalf("carol: %q %v", out, err)
	}
	if body := f.prs[2]["body"].(string); !strings.Contains(body, "Asked by carol (not in homepage) through homepage/dev.") {
		t.Errorf("body: %q", body)
	}
	if strings.Join(f.patched, ",") != "bob,agent-homepage-dev" {
		t.Errorf("patched as %v", f.patched)
	}
	// Without that grant, carol gets nothing.
	other := &agents.Agent{Namespace: "homepage", Name: "other", Tools: []string{"propose_patch"}}
	if _, err := propose(carol, other, "dev", ""); err == nil || !strings.Contains(err.Error(), "has no grant") {
		t.Errorf("carol through other: %v", err)
	}
	// Adding to carol's change, as bob, notes him.
	branch := f.prs[2]["head"].(map[string]any)["ref"].(string)
	if _, err := propose(bob, dev, "dev", branch); err != nil {
		t.Fatal(err)
	}
	if len(f.comments[2]) != 1 || !strings.HasPrefix(f.comments[2][0], "Changes added, asked by bob through homepage/dev.") {
		t.Errorf("comments: %q", f.comments[2])
	}

	// Merging carol's work into dev: carol never merges; bob may by the
	// grant, but it holds carol's work, so someone who did not ask must
	// approve it first.
	if _, err := merge(carol, 2); err == nil || !strings.Contains(err.Error(), "no grant to merge") {
		t.Errorf("carol merged: %v", err)
	}
	if _, err := merge(bob, 2); err == nil || !strings.Contains(err.Error(), "asked for by carol") {
		t.Errorf("bob merged carol's work unreviewed: %v", err)
	}
	f.reviews[2] = []map[string]any{{"user": map[string]any{"login": "alice"}, "state": "APPROVED"}}
	if out, err := merge(bob, 2); err != nil || !strings.Contains(out, "Merged homepage/todo#2 into dev with the account agent-homepage-dev") {
		t.Errorf("bob's merge: %q %v", out, err)
	}
	// bob's own change into main: only admins, and only with ci green.
	f.prs[1]["base"] = map[string]any{"ref": "main"}
	if _, err := merge(bob, 1); err == nil || !strings.Contains(err.Error(), "no grant to merge into main") {
		t.Errorf("bob merged into main: %v", err)
	}
	f.status = "failure"
	if _, err := merge(alice, 1); err == nil || !strings.Contains(err.Error(), "ci is failure") {
		t.Errorf("merged with a failing check: %v", err)
	}
	f.status = "success"
	// It is bob's work, not alice's: she needs someone else's approval too.
	if _, err := merge(alice, 1); err == nil || !strings.Contains(err.Error(), "asked for by bob") {
		t.Errorf("alice merged bob's work unreviewed: %v", err)
	}
	// Her own change she may put live herself.
	if _, err := propose(alice, dev, "main", ""); err != nil {
		t.Fatal(err)
	}
	if out, err := merge(alice, 3); err != nil || !strings.Contains(out, "deploys") {
		t.Errorf("alice's own merge: %q %v", out, err)
	}
	if strings.Join(f.merged, ",") != "2 by agent-homepage-dev,3 by agent-homepage-dev" {
		t.Errorf("merged: %v", f.merged)
	}

	// bob pushes onto alice's next change himself, in Gitea, past the
	// gateway: it holds his work now, and alice does not put it live alone.
	out, err = propose(alice, dev, "main", "")
	if err != nil {
		t.Fatal(err)
	}
	hers := f.prs[len(f.prs)]
	f.feed = []map[string]any{{"id": 1, "op_type": "commit_repo", "ref_name": "refs/heads/" + hers["head"].(map[string]any)["ref"].(string), "act_user": map[string]any{"login": "bob"}}}
	if _, err := merge(alice, hers["number"].(int)); err == nil || !strings.Contains(err.Error(), "asked for by bob") {
		t.Errorf("alice merged what bob pushed onto her change: %v", err)
	}
	delete(f.prs, hers["number"].(int))

	// A person may be called agent-something. Their own pull request, made
	// in Gitea, is theirs all the same: bob does not put it live unreviewed.
	f.prs[9] = map[string]any{
		"number": 9, "state": "open", "body": "", "html_url": "https://git/homepage/todo/pulls/9",
		"user": map[string]any{"login": "agent-smith"}, "base": map[string]any{"ref": "dev"},
		"head": map[string]any{"ref": "change/agent-smith-1", "sha": "sha9", "repo": map[string]any{"full_name": "homepage/todo"}},
	}
	if _, err := merge(bob, 9); err == nil || !strings.Contains(err.Error(), "asked for by agent-smith") {
		t.Errorf("bob merged agent-smith's work unreviewed: %v", err)
	}
	delete(f.prs, 9)

	// Without a person: the grant must allow it, for the routine's owner.
	night := Person{Name: AgentAccount(dev), Unattended: true, Owner: "bob", StartedBy: "routine homepage/dev/nightly"}
	if _, err := propose(night, dev, "dev", ""); err == nil || !strings.Contains(err.Error(), "unattended: true") {
		t.Errorf("unattended without the grant: %v", err)
	}
	dev.Grants[0].Unattended = true
	if out, err := propose(night, dev, "dev", ""); err != nil || !strings.Contains(out, "Branch: agent/dev/bob-") {
		t.Errorf("unattended: %q %v", out, err)
	}
	if body := f.prs[4]["body"].(string); !strings.Contains(body, "Asked by bob through homepage/dev, routine homepage/dev/nightly, with nobody watching.") {
		t.Errorf("unattended body: %q", body)
	}
	if _, err := tools.Call(ctx, night, dev, "screenshot", json.RawMessage(`{}`)); err == nil {
		t.Error("a screenshot without a person")
	}
}

func TestAgentAccountNames(t *testing.T) {
	if n := AgentAccount(&agents.Agent{Namespace: "homepage", Name: "dev"}); n != "agent-homepage-dev" {
		t.Error(n)
	}
	long := AgentAccount(&agents.Agent{Namespace: "a-very-long-namespace-name", Name: "and-a-long-agent"})
	if len(long) > 40 || !strings.HasPrefix(long, "agent-") {
		t.Error(long)
	}
}

func TestServiceKeyRuns(t *testing.T) {
	m, _ := setup(t)
	m.ServiceKey = ServiceKey("pw")
	m.Agents = &Catalog{}
	m.Agents.Set([]agents.Agent{{Namespace: "homepage", Name: "dev", Tools: []string{"list_repos"}, Routines: []agents.Routine{{Name: "n"}}}})
	call := func(h map[string]string) int {
		body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list"})
		r := httptest.NewRequest(http.MethodPost, "/mcp/homepage/dev", strings.NewReader(string(body)))
		for k, v := range h {
			r.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		m.ServeHTTP(w, r)
		return w.Code
	}
	if c := call(map[string]string{ServiceHeader: m.ServiceKey, OwnerHeader: "bob"}); c != http.StatusOK {
		t.Errorf("unattended run: %d", c)
	}
	if c := call(map[string]string{ServiceHeader: "wrong", OwnerHeader: "bob"}); c != http.StatusUnauthorized {
		t.Errorf("with a wrong key: %d", c)
	}
	if c := call(map[string]string{OwnerHeader: "bob"}); c != http.StatusUnauthorized {
		t.Errorf("without the key: %d", c)
	}
	m.ServiceKey = ""
	if c := call(map[string]string{ServiceHeader: "", OwnerHeader: "bob"}); c != http.StatusUnauthorized {
		t.Errorf("with no key configured: %d", c)
	}
}

func TestPrivateReposAreForTheNamespacesPeople(t *testing.T) {
	f := newGrantsGitea()
	srv := httptest.NewServer(f)
	defer srv.Close()
	tools := &Tools{Gitea: &Gitea{URL: srv.URL, User: "admin", Password: "pw"}}
	desk := &agents.Agent{Namespace: "homepage", Name: "desk", Tools: []string{"propose_patch", "merge_change"}, Grants: []agents.Grant{
		{Repo: "secret", Level: agents.LevelPropose, For: agents.ForEveryone},
		{Repo: "secret", Branch: "dev", Level: agents.LevelAutonomous, For: agents.ForMembers},
	}}
	ctx := context.Background()
	raw, _ := json.Marshal(map[string]any{"namespace": "homepage", "repo": "secret", "patch": "diff --git a/x b/x\n", "title": "T"})
	if _, err := tools.Call(ctx, Person{Name: "carol"}, desk, "propose_patch", raw); err == nil || !strings.Contains(err.Error(), "carol cannot see homepage/secret") {
		t.Errorf("carol proposed to a repo she cannot see: %v", err)
	}
	raw, _ = json.Marshal(map[string]any{"namespace": "homepage", "repo": "secret", "number": 1})
	if _, err := tools.Call(ctx, Person{Name: "carol"}, desk, "merge_change", raw); err == nil || !strings.Contains(err.Error(), "cannot see") {
		t.Errorf("carol learnt about a pull request she cannot see: %v", err)
	}
	problems, err := tools.Gitea.grantProblems(ctx, "homepage", []agents.Agent{*desk})
	if err != nil || len(problems) != 1 || !strings.Contains(problems[0], "secret is private") {
		t.Errorf("approval check: %v %v", problems, err)
	}
	if private, _ := tools.Gitea.AgentPrivate(ctx, desk); !private {
		t.Error("an agent with a grant on a private repo is not private")
	}
}

func TestPrivateAgentsReachOnlyTheRegistries(t *testing.T) {
	f := newGrantsGitea()
	srv := httptest.NewServer(f)
	defer srv.Close()
	g := &Gitea{URL: srv.URL, User: "admin", Password: "pw"}
	list := []agents.Agent{
		{Namespace: "homepage", Name: "wide", Repos: []string{"secret"}, Internet: agents.Internet{All: true}},
		{Namespace: "homepage", Name: "listed", Visible: agents.VisibleMembers, Internet: agents.Internet{Domains: []string{"example.org"}}},
		{Namespace: "homepage", Name: "builder", Repos: []string{"secret"}, Internet: agents.Internet{Packages: true}},
		{Namespace: "homepage", Name: "offline", Repos: []string{"secret"}},
	}
	problems, err := g.grantProblems(context.Background(), "homepage", list)
	if err != nil {
		t.Fatal(err)
	}
	if len(problems) != 2 || !strings.HasPrefix(problems[0], "agents/wide: internet:") || !strings.HasPrefix(problems[1], "agents/listed: internet:") ||
		!strings.Contains(problems[0], "internet: packages") {
		t.Errorf("problems %q", problems)
	}
}
