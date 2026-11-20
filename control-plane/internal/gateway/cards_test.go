package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"garrison/control-plane/internal/changes"
)

// A Gitea with todo/config#4 by alice and lab/app#2 by dave; bob is an
// admin of todo, not of lab. Everything is asked as the person.
func cardsGitea(t *testing.T, merges *[]string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		who := r.Header.Get("Sudo")
		send := func(v any) { json.NewEncoder(w).Encode(v) }
		switch r.Method + " " + r.URL.Path {
		case "GET /api/v1/repos/issues/search":
			send([]map[string]any{
				{"number": 4, "user": map[string]string{"login": "alice"}, "repository": map[string]string{"owner": "todo", "name": "config"}},
				{"number": 2, "user": map[string]string{"login": "dave"}, "repository": map[string]string{"owner": "lab", "name": "app"}},
			})
		case "GET /api/v1/repos/todo/config/raw/members.yaml":
			w.Write([]byte("admins: [alice, bob]\n"))
		case "GET /api/v1/repos/lab/config/raw/members.yaml":
			w.Write([]byte("admins: [dave]\nmembers: [bob]\n"))
		case "GET /api/v1/repos/todo/config/pulls/4":
			if who == "eve" {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			send(map[string]any{"number": 4, "title": "A screenshot every morning", "body": "why", "state": "open",
				"user": map[string]string{"login": "alice"}, "base": map[string]string{"ref": "main"}, "head": map[string]string{"sha": "abc"},
				"html_url": "https://git/todo/config/pulls/4"})
		case "GET /api/v1/repos/todo/config/branch_protections/main":
			send(map[string]any{"rule_name": "main", "required_approvals": 1})
		case "GET /api/v1/repos/todo/config/pulls/4/files":
			send([]map[string]string{{"filename": "routines.yaml", "status": "modified"}})
		case "GET /api/v1/repos/todo/config/pulls/4/reviews":
			send([]map[string]any{
				{"state": "APPROVED", "user": map[string]string{"login": "carol"}, "stale": true},
				{"state": "APPROVED", "user": map[string]string{"login": "dave"}},
				{"state": "REQUEST_CHANGES", "user": map[string]string{"login": "dave"}},
				{"state": "COMMENT", "user": map[string]string{"login": "erin"}},
				{"state": "APPROVED", "user": map[string]string{"login": "frank"}},
			})
		case "GET /api/v1/repos/todo/config/contents/stacks":
			http.NotFound(w, r) // no apps: the merge deletes the branch
		case "GET /api/v1/repos/todo/config/commits/abc/status":
			send(map[string]any{"statuses": []map[string]string{{"context": "garrison/approval", "status": "pending", "description": "waits"}}})
		case "POST /api/v1/repos/todo/config/pulls/4/merge":
			*merges = append(*merges, who)
			if len(*merges) < 2 {
				w.WriteHeader(http.StatusMethodNotAllowed)
				w.Write([]byte(`{"message":"Please try again later"}`))
				return
			}
		default:
			t.Errorf("unexpected %s %s as %q", r.Method, r.URL.Path, who)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func TestCardsAsThePersonSeesThem(t *testing.T) {
	var merges []string
	srv := cardsGitea(t, &merges)
	defer srv.Close()
	g := &Gitea{URL: srv.URL, User: "root", Password: "pw"}
	ctx := context.Background()

	c, err := g.Card(ctx, "bob", "todo", "config", 4)
	if err != nil {
		t.Fatal(err)
	}
	// A stale approval and one taken back by a request for changes do not count.
	if c.Title != "A screenshot every morning" || c.Author != "alice" || c.Mine || c.Approved || c.NoApproval || !slices.Equal(c.Approvals, []string{"frank"}) ||
		len(c.Files) != 1 || c.Files[0].Path != "routines.yaml" || c.CheckState() != "running" || c.URL != "https://git/todo/config/pulls/4" {
		t.Errorf("card: %+v", c)
	}
	if _, err := g.Card(ctx, "eve", "todo", "config", 4); !errors.Is(err, changes.ErrForbidden) {
		t.Errorf("eve: %v", err)
	}
	if _, err := g.Card(ctx, "bob", "../x", "config", 4); err == nil {
		t.Error("a bad unit was asked for")
	}

	// bob is todo's admin, so alice's change waits for him; lab's is not his to approve.
	list, err := g.Waiting(ctx, "bob", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Unit != "todo" || list[0].Number != 4 {
		t.Errorf("bob waits for %+v", list)
	}
	// alice's own waits for her too.
	if list, _ := g.Waiting(ctx, "alice", 10); len(list) != 1 || !list[0].Mine {
		t.Errorf("alice waits for %+v", list)
	}

	// While Gitea still checks it, the merge is tried again.
	if err := g.Merge(ctx, "alice", "todo", "config", 4); err != nil || strings.Join(merges, ",") != "alice,alice" {
		t.Errorf("merge: %v, tried as %v", err, merges)
	}
}

func TestAChangeOpenedLinksItsCard(t *testing.T) {
	c := Change{Unit: "todo", Repo: "config", Number: 4, URL: "https://git.example.com/todo/config/pulls/4"}
	got := (&Tools{Dashboard: "https://example.com"}).links(c)
	if !strings.Contains(got, "\nReview it: https://example.com/c/todo/config/4\nIn Gitea: https://git.example.com/todo/config/pulls/4") {
		t.Errorf("with a dashboard: %q", got)
	}
	if got := (&Tools{}).links(c); !strings.HasSuffix(got, ":\nhttps://git.example.com/todo/config/pulls/4") {
		t.Errorf("without: %q", got)
	}
}
