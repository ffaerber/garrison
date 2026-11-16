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
	"time"
)

func TestParseRoster(t *testing.T) {
	r, err := ParseRoster([]byte("# comment\nadmins: [alice]\nmembers:\n  - bob\n"))
	if err != nil || r.Role("alice") != "admin" || r.Role("bob") != "member" || r.Role("carol") != "" {
		t.Fatalf("roster = %+v, err %v", r, err)
	}
	for name, doc := range map[string]string{
		"no admin":       "admins: []\nmembers: [bob]\n",
		"listed twice":   "admins: [alice]\nmembers: [alice]\n",
		"not a username": "admins: [\"alice; rm\"]\n",
		"unknown key":    "admins: [alice]\nowners: [bob]\n",
		"not yaml":       "admins: [alice\n",
	} {
		if _, err := ParseRoster([]byte(doc)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestChanged(t *testing.T) {
	before := Roster{Admins: []string{"alice"}, Members: []string{"bob"}}
	after := Roster{Admins: []string{"alice", "bob"}, Members: []string{"carol"}}
	if got := strings.Join(Changed(before, after), " "); got != "bob carol" {
		t.Errorf("changed = %q", got)
	}
	if len(Changed(before, before)) != 0 {
		t.Error("no change reported as change")
	}
}

func TestApproval(t *testing.T) {
	base := Roster{Admins: []string{"alice", "carol"}, Members: []string{"bob"}}
	makeBobAdmin := Roster{Admins: []string{"alice", "carol", "bob"}}
	demoteCarol := Roster{Admins: []string{"alice"}, Members: []string{"bob", "carol"}}
	roots := []string{"admin", "alice"}
	for _, c := range []struct {
		name      string
		head      Roster
		author    string
		approvers []string
		ok        bool
		pushers   []string
	}{
		{"nobody approved", makeBobAdmin, "bob", nil, false, nil},
		{"bob approves his own promotion", makeBobAdmin, "bob", []string{"bob"}, false, nil},
		{"another admin approves bob's promotion", makeBobAdmin, "bob", []string{"carol"}, true, nil},
		{"the author approves", demoteCarol, "alice", []string{"alice"}, false, nil},
		{"carol approves her own demotion", demoteCarol, "alice", []string{"carol"}, false, nil},
		{"a member approves", demoteCarol, "alice", []string{"bob"}, false, nil},
		{"a root admin approves", demoteCarol, "alice", []string{"admin"}, true, nil},
		{"one good approval among bad ones", demoteCarol, "alice", []string{"carol", "bob", "admin"}, true, nil},
		// carol pushed onto bob's pull request: her approval is of her own work.
		{"an admin who pushed to it approves", makeBobAdmin, "bob", []string{"carol"}, false, []string{"carol"}},
		{"another admin approves what carol pushed to", makeBobAdmin, "bob", []string{"carol", "alice"}, true, []string{"bob", "carol"}},
	} {
		ok, why := Approval(base, c.head, append([]string{c.author}, c.pushers...), c.approvers, roots)
		if ok != c.ok {
			t.Errorf("%s: %v (%s)", c.name, ok, why)
		}
	}
	_, why := Approval(base, demoteCarol, []string{"alice"}, []string{"carol"}, roots)
	if !strings.Contains(why, "carol's own role changes") || !strings.Contains(why, "changes: carol") {
		t.Errorf("reason = %q", why)
	}
}

func TestSameRule(t *testing.T) {
	want := map[string]any{
		"rule_name": "main", "enable_push": false, "required_approvals": 1,
		"merge_whitelist_teams": []string{"admins", RootTeam}, "merge_whitelist_usernames": []string{},
	}
	var have map[string]any
	json.Unmarshal([]byte(`{"rule_name":"main","enable_push":false,"required_approvals":1,
		"merge_whitelist_teams":["root-admins","admins"],"merge_whitelist_usernames":[],"created_at":"x"}`), &have)
	if !sameRule(want, have) {
		t.Fatal("same rule seen as different")
	}
	have["required_approvals"] = float64(2)
	if sameRule(want, have) {
		t.Fatal("changed approvals not seen")
	}
	have["required_approvals"] = float64(1)
	have["merge_whitelist_teams"] = []any{"admins"}
	if sameRule(want, have) {
		t.Fatal("missing team not seen")
	}
	delete(have, "merge_whitelist_teams")
	if sameRule(want, have) {
		t.Fatal("missing setting not seen")
	}
}

// A team's members are read page by page: someone taken out of
// members.yaml is removed from the team wherever Gitea lists them.
func TestTeamMembersBeyondTheFirstPage(t *testing.T) {
	var removed []string
	gitea := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/api/v1")
		switch {
		case p == "/teams/7/members" && r.Method == http.MethodGet:
			var page []map[string]string
			switch r.URL.Query().Get("page") {
			case "1", "":
				for i := range 50 {
					page = append(page, map[string]string{"login": fmt.Sprintf("p%02d", i)})
				}
			case "2":
				page = append(page, map[string]string{"login": "zoe"})
			}
			json.NewEncoder(w).Encode(page)
		case strings.HasPrefix(p, "/teams/7/members/") && r.Method == http.MethodDelete:
			removed = append(removed, strings.TrimPrefix(p, "/teams/7/members/"))
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer gitea.Close()
	m := &Members{Gitea: &Gitea{URL: gitea.URL, User: "admin", Password: "pw"}}
	var want []string
	for i := range 50 {
		want = append(want, fmt.Sprintf("p%02d", i))
	}
	// zoe is no longer in members.yaml.
	if err := m.syncTeam(context.Background(), "homepage", "members", 7, want); err != nil {
		t.Fatal(err)
	}
	if len(removed) != 1 || removed[0] != "zoe" {
		t.Errorf("removed %v, want zoe", removed)
	}
}

// An admin who pushes onto someone else's pull request on config cannot be
// the one who approves it: the approval would be of their own work.
func TestWhoPushedToAConfigChangeDoesNotApproveIt(t *testing.T) {
	var mu sync.Mutex
	approvers := []string{"bob"}
	feed := []map[string]any{
		// Newest first. An earlier branch of the same name, by dave, was deleted.
		{"id": 4, "op_type": "commit_repo", "ref_name": "refs/heads/change/alice-1", "act_user": map[string]any{"login": "bob"}},
		{"id": 3, "op_type": "commit_repo", "ref_name": "refs/heads/change/alice-1", "act_user": map[string]any{"login": "alice"}},
		{"id": 2, "op_type": "delete_branch", "ref_name": "refs/heads/change/alice-1", "act_user": map[string]any{"login": "dave"}},
		{"id": 1, "op_type": "commit_repo", "ref_name": "refs/heads/change/alice-1", "act_user": map[string]any{"login": "dave"}},
	}
	var statuses []string
	gitea := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		p := strings.TrimPrefix(r.URL.Path, "/api/v1")
		switch {
		case p == "/orgs/homepage":
			io.WriteString(w, "{}")
		case p == "/repos/homepage/config/pulls" && r.Method == http.MethodGet:
			io.WriteString(w, `[{"number":5,"user":{"login":"alice"},"head":{"ref":"change/alice-1","sha":"abc","repo":{"full_name":"homepage/config"}}}]`)
		case p == "/repos/homepage/config/raw/members.yaml":
			io.WriteString(w, "admins: [bob, carol, dave]\nmembers: [alice]\n")
		case p == "/repos/homepage/config/pulls/5/reviews":
			var out []map[string]any
			for _, a := range approvers {
				out = append(out, map[string]any{"user": map[string]any{"login": a}, "state": "APPROVED"})
			}
			json.NewEncoder(w).Encode(out)
		case p == "/repos/homepage/config/activities/feeds":
			out := []map[string]any{}
			if r.URL.Query().Get("page") == "1" {
				out = feed
			}
			json.NewEncoder(w).Encode(out)
		case p == "/repos/homepage/config/statuses/abc" && r.Method == http.MethodPost:
			var body map[string]string
			json.NewDecoder(r.Body).Decode(&body)
			statuses = append(statuses, body["state"]+": "+body["description"])
			io.WriteString(w, "{}")
		default:
			http.NotFound(w, r) // no agents, stacks or restores
		}
	}))
	defer gitea.Close()
	m := &Members{Gitea: &Gitea{URL: gitea.URL, User: "admin", Password: "pw"}, posted: map[string]string{}}
	base := Roster{Admins: []string{"bob", "carol", "dave"}, Members: []string{"alice"}}
	last := func() string {
		t.Helper()
		if err := m.approvals(context.Background(), "homepage", &base, nil); err != nil {
			t.Fatal(err)
		}
		mu.Lock()
		defer mu.Unlock()
		if len(statuses) == 0 {
			return ""
		}
		return statuses[len(statuses)-1]
	}
	// bob pushed to alice's change, then approved it.
	if got := last(); !strings.HasPrefix(got, "pending: ") || !strings.Contains(got, "bob pushed to it") {
		t.Fatalf("bob's approval of what he pushed: %q", got)
	}
	// dave pushed only to an earlier branch of that name: his approval counts.
	mu.Lock()
	approvers = []string{"bob", "dave"}
	mu.Unlock()
	if got := last(); got != "success: approved by dave" {
		t.Errorf("dave's approval: %q", got)
	}
}

// main of a namespace's app repos is free for one person, and takes only
// approved pull requests once there are two; config keeps its own rule.
func TestAppReposOfSeveralPeopleNeedApprovals(t *testing.T) {
	rules := map[string]map[string]any{} // repo -> its rule for main
	var writes []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/api/v1")
		if p == "/orgs/lab/repos" {
			if r.URL.Query().Get("page") == "1" {
				io.WriteString(w, `[{"name":"config"},{"name":"example"},{"name":"notes"}]`)
			} else {
				io.WriteString(w, `[]`)
			}
			return
		}
		repo, rest, _ := strings.Cut(strings.TrimPrefix(p, "/repos/lab/"), "/")
		switch {
		case rest == "branch_protections/main" && r.Method == http.MethodGet:
			if rules[repo] == nil {
				http.NotFound(w, r)
				return
			}
			json.NewEncoder(w).Encode(rules[repo])
		case rest == "branch_protections" && r.Method == http.MethodPost, rest == "branch_protections/main" && r.Method == http.MethodPatch:
			var rule map[string]any
			json.NewDecoder(r.Body).Decode(&rule)
			rule["rule_name"] = "main"
			rules[repo] = rule
			writes = append(writes, r.Method+" "+repo)
			io.WriteString(w, "{}")
		case rest == "branch_protections/main" && r.Method == http.MethodDelete:
			delete(rules, repo)
			writes = append(writes, "DELETE "+repo)
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	m := &Members{Gitea: &Gitea{URL: srv.URL, User: "admin", Password: "pw"}}
	ctx := context.Background()

	// alice alone: nothing to protect.
	if err := m.protectApps(ctx, "lab", Roster{Admins: []string{"alice"}}); err != nil || len(writes) != 0 {
		t.Fatalf("alone: %v %v", writes, err)
	}
	// bob joins: main of each app repo, not config, takes approved pull requests only.
	if err := m.protectApps(ctx, "lab", Roster{Admins: []string{"alice"}, Members: []string{"bob"}}); err != nil {
		t.Fatal(err)
	}
	if strings.Join(writes, ",") != "POST example,POST notes" || rules["config"] != nil {
		t.Fatalf("protected: %v", writes)
	}
	r := rules["example"]
	if r["enable_push"] != false || r["required_approvals"] != float64(1) || r["dismiss_stale_approvals"] != true || r["block_admin_merge_override"] != true {
		t.Errorf("rule: %v", r)
	}
	// Again: unchanged, not written.
	m.protectApps(ctx, "lab", Roster{Admins: []string{"alice"}, Members: []string{"bob"}})
	if len(writes) != 2 {
		t.Errorf("written again: %v", writes)
	}
	// Someone loosens it by hand: put back.
	rules["notes"]["enable_push"] = true
	m.protectApps(ctx, "lab", Roster{Admins: []string{"alice", "bob"}})
	if writes[len(writes)-1] != "PATCH notes" || rules["notes"]["enable_push"] != false {
		t.Errorf("not put back: %v", writes)
	}
	// bob leaves: free again. Being listed twice does not make two people.
	m.protectApps(ctx, "lab", Roster{Admins: []string{"alice"}, Members: []string{"alice"}})
	if len(rules) != 0 {
		t.Errorf("still protected: %v", rules)
	}
}

// A config repo whose files did not go in is taken away again, so that the
// next pass makes it anew rather than take it as set up; files that go in
// on a second try keep it.
func TestAConfigRepoWithoutItsFilesGoesAgain(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	failures := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		p := strings.TrimPrefix(r.URL.Path, "/api/v1")
		calls = append(calls, r.Method+" "+p)
		switch {
		case p == "/orgs/lab/repos" && r.Method == http.MethodPost:
			w.WriteHeader(http.StatusCreated)
			io.WriteString(w, "{}")
		case p == "/repos/lab/config/contents" && failures > 0:
			failures--
			http.Error(w, `{"message":"push rejected"}`, http.StatusInternalServerError)
		case p == "/repos/lab/config/contents":
			w.WriteHeader(http.StatusCreated)
			io.WriteString(w, "{}")
		case p == "/repos/lab/config" && r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	m := &Members{Gitea: &Gitea{URL: srv.URL, User: "admin", Password: "pw"}, RetryWait: time.Millisecond}
	files := map[string]string{"members.yaml": "admins: [alice]\n"}
	ctx := context.Background()

	failures = 1
	if err := m.createConfig(ctx, "lab", "d", "m", files); err != nil {
		t.Fatalf("second try: %v", err)
	}
	if strings.Join(calls, ",") != "POST /orgs/lab/repos,POST /repos/lab/config/contents,POST /repos/lab/config/contents" {
		t.Fatalf("calls: %v", calls)
	}

	calls, failures = nil, 3
	if err := m.createConfig(ctx, "lab", "d", "m", files); err == nil || !strings.Contains(err.Error(), "taken away again") {
		t.Fatalf("err: %v", err)
	}
	if last := calls[len(calls)-1]; last != "DELETE /repos/lab/config" || len(calls) != 5 {
		t.Fatalf("calls: %v", calls)
	}
}
