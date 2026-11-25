package home

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"

	"garrison/control-plane/internal/events"
	"garrison/control-plane/internal/secrets"
)

// fakeSecrets: alice manages todo; values are kept to check what was set.
type fakeSecrets struct {
	values map[string]string // ns/name
}

func (f *fakeSecrets) May(_ context.Context, ns, person string) (bool, error) {
	return person == "alice" && ns == "todo", nil
}

func (f *fakeSecrets) Units(ctx context.Context, person string) ([]string, error) {
	if ok, _ := f.May(ctx, "todo", person); ok {
		return []string{"todo"}, nil
	}
	return nil, nil
}

func (f *fakeSecrets) Names(_ context.Context, ns string) ([]string, error) {
	var out []string
	for k := range f.values {
		if n, ok := strings.CutPrefix(k, ns+"/"); ok {
			out = append(out, n)
		}
	}
	slices.Sort(out)
	return out, nil
}

func (f *fakeSecrets) Set(ctx context.Context, ns, name, value, person string) error {
	if ok, _ := f.May(ctx, ns, person); !ok {
		return secrets.ErrForbidden
	}
	if err := secrets.CheckValue(value); err != nil {
		return err
	}
	f.values[ns+"/"+name] = value
	return nil
}

func (f *fakeSecrets) Delete(ctx context.Context, ns, name, person string) error {
	if ok, _ := f.May(ctx, ns, person); !ok {
		return secrets.ErrForbidden
	}
	if _, ok := f.values[ns+"/"+name]; !ok {
		return fmt.Errorf("%w: no %s", secrets.ErrInvalid, name)
	}
	delete(f.values, ns+"/"+name)
	return nil
}

func post(s *Server, who, origin, path string, form url.Values) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", path, strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set(UserHeader, who)
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w
}

func TestTheSecretsPage(t *testing.T) {
	s, m := server()
	f := &fakeSecrets{values: map[string]string{"todo/db-password": "hunter2"}}
	s.Secrets = f
	origin := "https://home.test"

	// alice sees the names, never a value.
	w := get(s, "alice", "/secrets")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "db-password") || strings.Contains(w.Body.String(), "hunter2") {
		t.Fatalf("page: %d\n%s", w.Code, w.Body)
	}
	// dave manages none.
	if w := get(s, "dave", "/secrets"); !strings.Contains(w.Body.String(), "you are neither") {
		t.Errorf("dave's page:\n%s", w.Body)
	}

	// Setting one redirects to the page, which says so, and the feed has it
	// for the unit's people.
	w = post(s, "alice", origin, "/secrets/todo", url.Values{"action": {"set"}, "name": {"shop-api-key"}, "value": {"tok-123"}})
	if w.Code != http.StatusSeeOther || f.values["todo/shop-api-key"] != "tok-123" {
		t.Fatalf("set: %d %v", w.Code, f.values)
	}
	if w := get(s, "alice", w.Header().Get("Location")); !strings.Contains(w.Body.String(), "Saved shop-api-key") {
		t.Errorf("no notice after saving:\n%s", w.Body)
	}
	es, _ := m.List(context.Background(), events.Query{Visibility: events.Visibility{All: true}, Kinds: []string{events.KindSecret}})
	if len(es) != 1 || es[0].Actor != "alice" || es[0].Audience != events.AudienceMembers || strings.Contains(es[0].Title+es[0].Detail, "tok-123") {
		t.Errorf("feed: %+v", es)
	}

	// From another page, an app's say, nothing is taken.
	for _, o := range []string{"", "https://todo.home.test", "https://evil.example"} {
		if w := post(s, "alice", o, "/secrets/todo", url.Values{"action": {"set"}, "name": {"x"}, "value": {"y"}}); w.Code != http.StatusForbidden {
			t.Errorf("origin %q: %d", o, w.Code)
		}
	}
	if _, ok := f.values["todo/x"]; ok {
		t.Error("a cross-site post was taken")
	}

	// Refusals show on the page with the reason.
	if w := post(s, "dave", origin, "/secrets/todo", url.Values{"action": {"set"}, "name": {"x"}, "value": {"y"}}); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "admins") {
		t.Errorf("dave set one: %d\n%s", w.Code, w.Body)
	}
	if w := post(s, "alice", origin, "/secrets/todo", url.Values{"action": {"set"}, "name": {"empty"}, "value": {""}}); w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "empty") {
		t.Errorf("empty value: %d", w.Code)
	}

	// Deleting.
	if w := post(s, "alice", origin, "/secrets/todo", url.Values{"action": {"delete"}, "name": {"db-password"}}); w.Code != http.StatusSeeOther {
		t.Errorf("delete: %d", w.Code)
	}
	if _, ok := f.values["todo/db-password"]; ok {
		t.Error("not deleted")
	}
}

// A site login is the secret login-<name>, its site, username and password
// as JSON; the page never shows it back.
func TestASiteLogin(t *testing.T) {
	s, _ := server()
	f := &fakeSecrets{values: map[string]string{}}
	s.Secrets = f
	origin := "https://home.test"
	w := post(s, "alice", origin, "/secrets/todo", url.Values{"action": {"login"}, "login": {"shop"}, "site": {"Shop.Example.com/login"},
		"username": {"alice@shop"}, "password": {"pa55 word"}})
	if w.Code != http.StatusSeeOther {
		t.Fatalf("login: %d %s", w.Code, w.Body)
	}
	l, err := secrets.ParseLogin(f.values["todo/login-shop"])
	if err != nil || l.Site != "https://shop.example.com" || l.Username != "alice@shop" || l.Password != "pa55 word" {
		t.Errorf("stored %q: %+v %v", f.values["todo/login-shop"], l, err)
	}
	if body := get(s, "alice", "/secrets?ns=todo").Body.String(); !strings.Contains(body, "login-shop") || strings.Contains(body, "pa55") || !strings.Contains(body, `id="loginform"`) {
		t.Errorf("the page:\n%s", body)
	}
	for name, form := range map[string]url.Values{
		"no password":  {"action": {"login"}, "login": {"x"}, "site": {"https://a.example"}, "username": {"u"}},
		"a bad site":   {"action": {"login"}, "login": {"x"}, "site": {"ftp://a.example"}, "username": {"u"}, "password": {"p"}},
		"a bad name":   {"action": {"login"}, "login": {"Shop!"}, "site": {"https://a.example"}, "username": {"u"}, "password": {"p"}},
		"a user in it": {"action": {"login"}, "login": {"x"}, "site": {"https://me:pw@a.example"}, "username": {"u"}, "password": {"p"}},
	} {
		if w := post(s, "alice", origin, "/secrets/todo", form); w.Code != http.StatusBadRequest {
			t.Errorf("%s: %d", name, w.Code)
		}
	}
	if w := post(s, "dave", origin, "/secrets/todo", url.Values{"action": {"login"}, "login": {"x"}, "site": {"https://a.example"}, "username": {"u"}, "password": {"p"}}); w.Code != http.StatusForbidden {
		t.Errorf("dave: %d", w.Code)
	}
}

func TestLoginFor(t *testing.T) {
	l := secrets.Login{Site: "https://shop.example.com", Username: "u", Password: "p"}
	for u, want := range map[string]bool{
		"https://shop.example.com/login":     true,
		"https://www.shop.example.com/":      true,
		"http://shop.example.com/":           false,
		"https://shop.example.com.evil.org/": false,
		"https://evilshop.example.com/":      false,
		"https://example.com/":               false,
	} {
		if got := l.For(u); got != want {
			t.Errorf("For(%q) = %v", u, got)
		}
	}
}
