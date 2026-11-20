package home

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"garrison/control-plane/internal/changes"
	"garrison/control-plane/internal/events"
)

// fakeChanges: todo/config#4, alice's routine, waits for an approval; bob
// is todo's other admin, dave may not read it.
type fakeChanges struct {
	card     changes.Card
	approved []string
	merged   bool
	// checking: merges refused for running checks, after an approval
	checking int
}

func newFakeChanges() *fakeChanges {
	return &fakeChanges{card: changes.Card{Unit: "todo", Repo: "config", Number: 4, Title: "A screenshot every morning",
		Body: "Asked by alice through todo/operator.", Author: "alice", Base: "main", URL: "https://git.home.test/todo/config/pulls/4",
		Files:  []changes.File{{Path: "routines.yaml", Status: "modified"}, {Path: "grants.yaml", Status: "modified"}},
		Checks: []changes.Check{{Name: "garrison/approval", State: "pending", Detail: "waits for an admin of todo who did not write it"}}}}
}

func (f *fakeChanges) Card(_ context.Context, person, ns, repo string, n int) (changes.Card, error) {
	if person == "dave" {
		return changes.Card{}, changes.ErrForbidden
	}
	if ns != "todo" || repo != "config" || n != 4 {
		return changes.Card{}, errors.New("gitea GET /repos/x/pulls/9: 404 not found")
	}
	c := f.card
	c.Approvals = f.approved
	c.Merged = f.merged
	c.Mine = person == c.Author
	for _, a := range f.approved {
		c.Approved = c.Approved || a == person
	}
	if len(f.approved) > 0 {
		c.Checks = []changes.Check{{Name: "garrison/approval", State: "success"}}
	}
	return c, nil
}

func (f *fakeChanges) Waiting(ctx context.Context, person string, limit int) ([]changes.Card, error) {
	if person != "bob" && person != "alice" {
		return nil, nil
	}
	c, err := f.Card(ctx, person, "todo", "config", 4)
	return []changes.Card{c}, err
}

func (f *fakeChanges) Approve(_ context.Context, person, ns, repo string, n int) error {
	if person == "alice" {
		return errors.New("gitea POST /repos/todo/config/pulls/4/reviews: 422 approve your own pull is not allowed")
	}
	f.approved = append(f.approved, person)
	return nil
}

func (f *fakeChanges) Merge(_ context.Context, person, ns, repo string, n int) error {
	if len(f.approved) == 0 {
		return errors.New("gitea POST /repos/todo/config/pulls/4/merge: 405 Does not have enough approvals")
	}
	if f.checking > 0 {
		f.checking--
		return errors.New("gitea POST /repos/todo/config/pulls/4/merge: 405 Not all required status checks successful")
	}
	f.merged = true
	return nil
}

func TestAChangeWaitsOnTheDashboard(t *testing.T) {
	s, _ := server()
	s.Changes = newFakeChanges()
	body := get(s, "bob", "/").Body.String()
	for _, want := range []string{"Waiting for you", `href="/c/todo/config/4"`, "A screenshot every morning", "by alice", "It waits for an approval: yours"} {
		if !strings.Contains(body, want) {
			t.Errorf("bob's feed lacks %q", want)
		}
	}
	if body := get(s, "carol", "/").Body.String(); strings.Contains(body, "Waiting for you") {
		t.Error("carol waits for nothing, yet her feed says so")
	}
}

func TestWaitingRefreshesOnItsOwn(t *testing.T) {
	s, _ := server()
	s.Changes = newFakeChanges()
	if body := get(s, "bob", "/").Body.String(); !strings.Contains(body, `<div id="waiting-box">`) || !strings.Contains(body, `data-key="c/c/todo/config/4"`) {
		t.Error("bob's page has no box for what waits, or its change has no key")
	}
	w := get(s, "bob", "/waiting")
	body := w.Body.String()
	if w.Code != 200 || !strings.Contains(body, "Waiting for you") || !strings.Contains(body, `href="/c/todo/config/4"`) || strings.Contains(body, "<html") {
		t.Errorf("bob's /waiting: %d %q", w.Code, body)
	}
	if body := get(s, "carol", "/waiting").Body.String(); strings.TrimSpace(body) != "" {
		t.Errorf("carol waits for nothing, yet /waiting says %q", body)
	}
	if w := get(s, "", "/waiting"); w.Code != http.StatusUnauthorized {
		t.Errorf("/waiting without a person: %d", w.Code)
	}
}

func TestTheCardSaysItPlainly(t *testing.T) {
	s, _ := server()
	s.Changes = newFakeChanges()
	w := get(s, "bob", "/c/todo/config/4")
	if w.Code != 200 {
		t.Fatalf("status %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		"alice proposed it",
		"what agents do on their own", "what agents may do here (grants)",
		"Asked by alice through todo/operator.",
		"waits for an admin of todo who did not write it",
		`value="approve_merge"`, "Approve and merge", `value="approve"`, "See it in Gitea",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the card lacks %q", want)
		}
	}
	// Its author is offered no approval of her own.
	if body := get(s, "alice", "/c/todo/config/4").Body.String(); strings.Contains(body, `value="approve"`) || !strings.Contains(body, "You proposed it") {
		t.Error("alice's own change offers her to approve it")
	}
	// Who may not read it, or asks for one that is not there, finds nothing.
	for _, c := range []struct{ who, path string }{{"dave", "/c/todo/config/4"}, {"bob", "/c/todo/config/9"}, {"bob", "/c/todo/config/x"}} {
		if w := get(s, c.who, c.path); w.Code != http.StatusNotFound {
			t.Errorf("%s %s: status %d, want 404", c.who, c.path, w.Code)
		}
	}
}

func TestApproveAndMergeOnTheCard(t *testing.T) {
	s, m := server()
	f := newFakeChanges()
	s.Changes = f
	merge := url.Values{"action": {"merge"}}
	// Merging first is refused, in plain words.
	w := post(s, "alice", "https://home.test", "/c/todo/config/4", merge)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "It needs an approval first") || f.merged {
		t.Fatalf("an early merge: %d merged=%v\n%s", w.Code, f.merged, w.Body.String())
	}
	// alice cannot approve her own.
	if w := post(s, "alice", "https://home.test", "/c/todo/config/4", url.Values{"action": {"approve"}}); !strings.Contains(w.Body.String(), "someone else approves it") {
		t.Errorf("alice approving her own: %d\n%s", w.Code, w.Body.String())
	}
	// Another site cannot post for bob.
	if w := post(s, "bob", "https://shop.home.test", "/c/todo/config/4", url.Values{"action": {"approve"}}); w.Code != http.StatusForbidden || len(f.approved) > 0 {
		t.Fatalf("a post from an app: %d, approved by %v", w.Code, f.approved)
	}
	w = post(s, "bob", "https://home.test", "/c/todo/config/4", url.Values{"action": {"approve"}})
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/c/todo/config/4?done=approve" || len(f.approved) != 1 {
		t.Fatalf("bob approving: %d %q %v", w.Code, w.Header().Get("Location"), f.approved)
	}
	if body := get(s, "bob", "/c/todo/config/4?done=approve").Body.String(); !strings.Contains(body, "You approved it.") || strings.Contains(body, `value="approve"`) {
		t.Error("after approving, the card does not say so, or still offers it")
	}
	w = post(s, "alice", "https://home.test", "/c/todo/config/4", merge)
	if w.Code != http.StatusSeeOther || !f.merged {
		t.Fatalf("alice merging: %d", w.Code)
	}
	if body := get(s, "alice", "/c/todo/config/4?done=merge").Body.String(); !strings.Contains(body, "You merged it.") || strings.Contains(body, `value="merge"`) {
		t.Error("after merging, the card does not say so, or still offers it")
	}
	es, _ := m.List(context.Background(), events.Query{Visibility: events.Visibility{All: true}, Limit: 50})
	var said []string
	for _, e := range es {
		if e.Source == events.SourceHome {
			said = append(said, e.Title)
		}
	}
	if strings.Join(said, "|") != "alice merged todo/config#4 on the dashboard|bob approved todo/config#4 on the dashboard" {
		t.Errorf("the feed has %q", said)
	}
}

func TestApproveAndMergeInOneClick(t *testing.T) {
	s, _ := server()
	s.MergeWait = 50 * time.Millisecond
	f := newFakeChanges()
	f.checking = 2 // the approval check takes a moment
	s.Changes = f
	w := post(s, "bob", "https://home.test", "/c/todo/config/4", url.Values{"action": {"approve_merge"}})
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/c/todo/config/4?done=approve_merge" || !f.merged || len(f.approved) != 1 {
		t.Fatalf("approve and merge: %d %q merged=%v approved=%v", w.Code, w.Header().Get("Location"), f.merged, f.approved)
	}
	if body := get(s, "bob", "/c/todo/config/4?done=approve_merge").Body.String(); !strings.Contains(body, "You merged it.") {
		t.Error("the card does not say it is merged")
	}

	// Checks that never pass: it is approved, and says what still waits.
	f = newFakeChanges()
	f.checking = 1000
	s.Changes = f
	w = post(s, "bob", "https://home.test", "/c/todo/config/4", url.Values{"action": {"approve_merge"}})
	if body := w.Body.String(); w.Code != http.StatusConflict || f.merged || len(f.approved) != 1 ||
		!strings.Contains(body, "You approved it.") || !strings.Contains(body, "checks have not all passed") {
		t.Errorf("checks not passing: %d merged=%v\n%s", w.Code, f.merged, body)
	}
	// Its author is not offered it.
	if body := get(s, "alice", "/c/todo/config/4").Body.String(); strings.Contains(body, "approve_merge") {
		t.Error("alice is offered to approve her own")
	}
}
