package home

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"testing"

	"garrison/control-plane/internal/events"
)

type fakePeople struct {
	list      []Person
	passwords map[string]string
	proposed  []string
}

func (f *fakePeople) List(context.Context) ([]Person, error) { return f.list, nil }
func (f *fakePeople) Add(_ context.Context, name, email, pw string) error {
	if slices.ContainsFunc(f.list, func(p Person) bool { return p.Username == name }) {
		return fmt.Errorf("the username %s is taken", name)
	}
	f.list = append(f.list, Person{Username: name, Email: email, Enabled: true})
	f.passwords[name] = pw
	return nil
}
func (f *fakePeople) SetPassword(_ context.Context, name, pw string) error {
	f.passwords[name] = pw
	return nil
}
func (f *fakePeople) ProposeMember(_ context.Context, as, unit, person, role string) (int, error) {
	f.proposed = append(f.proposed, fmt.Sprintf("%s %s %s %s", as, unit, person, role))
	return 14, nil
}

// unitAdmins: dave is an admin of todo, alice a root admin.
type unitAdmins struct{ fakeSecrets }

func (u *unitAdmins) Units(_ context.Context, p string) ([]string, error) {
	switch p {
	case "alice":
		return []string{"garrison", "shop", "todo"}, nil
	case "dave":
		return []string{"todo"}, nil
	}
	return nil, nil
}
func (u *unitAdmins) May(_ context.Context, ns, p string) (bool, error) {
	return p == "alice" || (p == "dave" && ns == "todo"), nil
}

var linkIn = regexp.MustCompile(`https://home\.test/join\?token=([0-9a-f]{64})`)

func join(s *Server, method, path string, form url.Values) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(form.Encode()))
	if form != nil {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set("Origin", "https://home.test")
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w
}

func TestInvitesAndPasswordLinks(t *testing.T) {
	s, m := server()
	people := &fakePeople{list: []Person{{Username: "alice", Root: true, Enabled: true}, {Username: "dave", Enabled: true}}, passwords: map[string]string{}}
	invites := &events.MemoryInvites{}
	s.People, s.Invites, s.Secrets, s.PlatformUser = people, invites, &unitAdmins{}, "admin"
	origin := "https://home.test"

	// The page: alice may invite anyone; dave only into todo; bob not at all.
	body := get(s, "alice", "/people").Body.String()
	for _, want := range []string{`href="/people"`, `<option value="">none: only an account</option>`, `<option value="shop">`, "Password link", "<code>dave</code>"} {
		if !strings.Contains(body, want) {
			t.Errorf("alice's page has no %q", want)
		}
	}
	if strings.Contains(body, `<option value="garrison">`) {
		t.Error("garrison is offered as a unit to join")
	}
	body = get(s, "dave", "/people").Body.String()
	if !strings.Contains(body, `<option value="todo">`) || strings.Contains(body, "none: only an account") || strings.Contains(body, "Password link") {
		t.Errorf("dave's page:\n%s", body)
	}
	if body := get(s, "bob", "/people").Body.String(); !strings.Contains(body, "Only root admins and a unit's admins invite people") {
		t.Error("bob is offered to invite")
	}

	// Refused: dave into shop, or without a unit; a link from another site.
	for _, c := range []struct {
		who  string
		form url.Values
	}{
		{"dave", url.Values{"action": {"invite"}, "unit": {"shop"}, "role": {"member"}}},
		{"dave", url.Values{"action": {"invite"}, "unit": {""}}},
		{"bob", url.Values{"action": {"invite"}, "unit": {"todo"}, "role": {"member"}}},
		{"dave", url.Values{"action": {"reset"}, "username": {"alice"}}},
	} {
		if w := post(s, c.who, origin, "/people", c.form); w.Code != 403 {
			t.Errorf("%s %v: %d", c.who, c.form, w.Code)
		}
	}
	if w := post(s, "alice", "https://todo.home.test", "/people", url.Values{"action": {"invite"}}); w.Code != 403 {
		t.Errorf("another site: %d", w.Code)
	}

	// dave invites a member of todo; the link is shown once, its hash kept.
	w := post(s, "dave", origin, "/people", url.Values{"action": {"invite"}, "unit": {"todo"}, "role": {"member"}, "note": {"Carla"}})
	found := linkIn.FindStringSubmatch(w.Body.String())
	if w.Code != 200 || found == nil {
		t.Fatalf("invite: %d\n%s", w.Code, w.Body)
	}
	token := found[1]
	if body := get(s, "dave", "/people").Body.String(); strings.Contains(body, token) || !strings.Contains(body, "as member of <code>todo</code> · Carla") {
		t.Errorf("the open links:\n%s", body)
	}

	// The link: a wrong token shows nothing; the right one, its form.
	if w := join(s, "GET", "/join?token="+strings.Repeat("0", 64), nil); w.Code != 404 {
		t.Errorf("a wrong token: %d", w.Code)
	}
	w = join(s, "GET", "/join?token="+token, nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "dave invites you to work in todo, as member") {
		t.Fatalf("join page: %d\n%s", w.Code, w.Body)
	}
	if w.Header().Get("Referrer-Policy") != "same-origin" {
		t.Error("the link could leak through the referrer")
	}
	// Bad names and passwords; a taken name leaves the link open.
	for _, bad := range []url.Values{
		{"username": {"admin"}, "password": {"long enough pw"}, "again": {"long enough pw"}},
		{"username": {"agent-todo-coder"}, "password": {"long enough pw"}, "again": {"long enough pw"}},
		{"username": {"carla"}, "password": {"short"}, "again": {"short"}},
		{"username": {"dave"}, "password": {"long enough pw"}, "again": {"long enough pw"}},
	} {
		bad.Set("token", token)
		if w := join(s, "POST", "/join", bad); w.Code != 400 {
			t.Errorf("%v: %d", bad, w.Code)
		}
	}
	w = join(s, "POST", "/join", url.Values{"token": {token}, "username": {"carla"}, "password": {"long enough pw"}, "again": {"long enough pw"}})
	if w.Code != 200 || !strings.Contains(w.Body.String(), "Welcome, carla") || !strings.Contains(w.Body.String(), "dave proposes you as member of todo") {
		t.Fatalf("accept: %d\n%s", w.Code, w.Body)
	}
	if people.passwords["carla"] != "long enough pw" || len(people.proposed) != 1 || people.proposed[0] != "dave todo carla member" {
		t.Errorf("%v %v", people.passwords, people.proposed)
	}
	if !slices.ContainsFunc(people.list, func(p Person) bool { return p.Username == "carla" && p.Email == "carla@home.test" }) {
		t.Errorf("no email given: %v", people.list)
	}
	// Once only.
	if w := join(s, "POST", "/join", url.Values{"token": {token}, "username": {"carla2"}, "password": {"long enough pw"}, "again": {"long enough pw"}}); w.Code != 404 {
		t.Errorf("used twice: %d", w.Code)
	}
	es, _ := m.List(context.Background(), events.Query{Visibility: events.Visibility{All: true}, Kinds: []string{events.KindPeople}})
	if !slices.ContainsFunc(es, func(e events.Event) bool { return strings.HasPrefix(e.Title, "carla joined Garrison, invited by dave") }) {
		t.Errorf("the feed: %v", es)
	}

	// Revoking: dave revokes his own; bob cannot.
	w = post(s, "alice", origin, "/people", url.Values{"action": {"invite"}, "unit": {""}})
	token2 := linkIn.FindStringSubmatch(w.Body.String())[1]
	open, _ := invites.OpenInvites(context.Background(), true, nil, "")
	if len(open) != 1 {
		t.Fatalf("open: %v", open)
	}
	id := fmt.Sprint(open[0].ID)
	if w := post(s, "dave", origin, "/people", url.Values{"action": {"revoke"}, "id": {id}}); w.Code != 404 {
		t.Errorf("dave revoked alice's link: %d", w.Code)
	}
	if w := post(s, "alice", origin, "/people", url.Values{"action": {"revoke"}, "id": {id}}); w.Code != 303 {
		t.Errorf("revoke: %d", w.Code)
	}
	if w := join(s, "GET", "/join?token="+token2, nil); w.Code != 404 {
		t.Errorf("a revoked link: %d", w.Code)
	}

	// A password link, from a root admin: it sets carla's password, once.
	w = post(s, "alice", origin, "/people", url.Values{"action": {"reset"}, "username": {"carla"}})
	token3 := linkIn.FindStringSubmatch(w.Body.String())[1]
	if body := join(s, "GET", "/join?token="+token3, nil).Body.String(); !strings.Contains(body, "for <code>carla</code> to set a new password") || strings.Contains(body, `name="username"`) {
		t.Errorf("reset page:\n%s", body)
	}
	if w := join(s, "POST", "/join", url.Values{"token": {token3}, "username": {"alice"}, "password": {"another long pw"}, "again": {"another long pw"}}); w.Code != 200 {
		t.Fatalf("reset: %d\n%s", w.Code, w.Body)
	}
	if people.passwords["carla"] != "another long pw" || people.passwords["alice"] != "" {
		t.Errorf("passwords: %v", people.passwords)
	}
	if w := join(s, "POST", "/join", url.Values{"token": {token3}, "password": {"third long pw!"}, "again": {"third long pw!"}}); w.Code != 404 {
		t.Errorf("reset twice: %d", w.Code)
	}
}

func TestNoJoinWithoutPeople(t *testing.T) {
	s, _ := server()
	if w := join(s, "GET", "/join?token=x", nil); w.Code != http.StatusNotFound {
		t.Errorf("%d", w.Code)
	}
	if w := get(s, "alice", "/people"); w.Code != http.StatusNotFound && w.Code != http.StatusMethodNotAllowed {
		t.Errorf("people page without people: %d", w.Code)
	}
}
