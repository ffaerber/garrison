package gateway

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"garrison/control-plane/internal/agents"
)

// A login is used only by the agent logins.yaml names, for the people of
// the group it names (in a routine, only where it says unattended), and
// only one the deployer wrote; the agent gets an action, never the value.
func TestWhoMayUseALogin(t *testing.T) {
	gitea := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/repos/todo/config/raw/logins.yaml":
			io.WriteString(w, "- login: shop\n  agent: scout\n  for: members\n- login: bank\n  agent: scout\n  for: admins\n  unattended: true\n")
		case "/api/v1/repos/todo/config/raw/members.yaml":
			io.WriteString(w, "admins: [alice]\nmembers: [bob]\n")
		default:
			http.NotFound(w, r)
		}
	}))
	defer gitea.Close()
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "todo"), 0o700)
	os.WriteFile(filepath.Join(dir, "todo", "shop.json"), []byte(`{"site":"https://shop.example.com","username":"todo-shop","password":"s3cret"}`), 0o600)
	os.WriteFile(filepath.Join(dir, "todo", "bank.json"), []byte(`{"site":"https://bank.example.com","username":"todo-bank","password":"b4nk"}`), 0o600)
	tools := &Tools{Gitea: &Gitea{URL: gitea.URL, User: "admin", Password: "pw"}, LoginsDir: dir}
	scout := &agents.Agent{Unit: "todo", Name: "scout"}
	coder := &agents.Agent{Unit: "todo", Name: "coder"}
	ctx := context.Background()
	for _, c := range []struct {
		who   Person
		agent *agents.Agent
		login string
		want  string // the username, or a part of the refusal
	}{
		{Person{Name: "bob"}, scout, "shop", "todo-shop"},
		{Person{Name: "alice"}, scout, "shop", "todo-shop"},
		{Person{Name: "carol"}, scout, "shop", "does not allow it"}, // not in todo
		{Person{Name: "bob"}, coder, "shop", "does not allow it"},   // another agent
		{Person{Name: "bob"}, scout, "bank", "does not allow it"},   // admins only
		{Person{Name: "alice"}, scout, "bank", "todo-bank"},
		{Person{Name: "agent-todo-scout", Unattended: true, Owner: "bob"}, scout, "shop", "does not allow it"}, // not unattended
		{Person{Name: "agent-todo-scout", Unattended: true, Owner: "alice"}, scout, "bank", "todo-bank"},
		{Person{Name: "bob"}, scout, "mail", "does not allow it"},
	} {
		l, err := tools.login(ctx, c.who, c.agent, c.login)
		got := l.Username
		if err != nil {
			got = err.Error()
		}
		if !strings.Contains(got, c.want) {
			t.Errorf("%s through %s, %s: %q, want %q", c.who.Name, c.agent.Name, c.login, got, c.want)
		}
	}
	// What browse gets: the password as the text of an action held to the
	// site and to a password field.
	act, err := tools.loginAction(ctx, Person{Name: "bob"}, scout, args{Login: "shop", Part: "password", TypeInto: 4})
	if err != nil || act.Text != "s3cret" || !act.Password || act.Site == nil || act.Site.Host() != "shop.example.com" || act.Type != 4 {
		t.Errorf("password: %+v %v", act, err)
	}
	if _, err := tools.loginAction(ctx, Person{Name: "bob"}, scout, args{Login: "shop", Part: "pin", TypeInto: 4}); err == nil {
		t.Error("a part that is not username or password")
	}
	if _, err := tools.loginAction(ctx, Person{Name: "bob"}, scout, args{Login: "shop"}); err == nil {
		t.Error("no field")
	}
	// A login logins.yaml allows but the deployer has not written.
	os.Remove(filepath.Join(dir, "todo", "shop.json"))
	if _, err := tools.login(ctx, Person{Name: "bob"}, scout, "shop"); err == nil || !strings.Contains(err.Error(), "has no login shop") {
		t.Errorf("unwritten: %v", err)
	}
	if _, err := ParseLogins("- login: x\n  agent: scout\n  for: friends\n"); err == nil {
		t.Error("for: friends")
	}
}
