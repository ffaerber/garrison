package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"garrison/control-plane/internal/agents"
)

// fakeKeycloak is the token endpoints and the bits of the admin API the
// gateway uses, with one realm's users and clients.
type fakeKeycloak struct {
	t        *testing.T
	mu       sync.Mutex
	clients  map[string]map[string]any // id -> representation
	mappers  []string
	users    map[string]map[string]any // username -> representation
	password map[string]string         // username -> password
	calls    []string
}

func newFakeKeycloak(t *testing.T) (*fakeKeycloak, *httptest.Server) {
	f := &fakeKeycloak{t: t, clients: map[string]map[string]any{}, users: map[string]map[string]any{}, password: map[string]string{}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *fakeKeycloak) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := r.URL.Path
	f.calls = append(f.calls, r.Method+" "+strings.TrimPrefix(p, "/admin/realms/garrison"))
	var body map[string]any
	if r.Header.Get("Content-Type") == "application/json" {
		json.NewDecoder(r.Body).Decode(&body)
	}
	switch {
	case p == "/realms/master/protocol/openid-connect/token":
		r.ParseForm()
		if r.Form.Get("username") != "root" || r.Form.Get("password") != "pw" || r.Form.Get("client_id") != "admin-cli" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"access_token": "admin", "expires_in": 60})
	case p == "/realms/garrison/protocol/openid-connect/token":
		r.ParseForm()
		u := f.users[r.Form.Get("username")]
		var secret any
		for _, c := range f.clients {
			if c["clientId"] == r.Form.Get("client_id") {
				secret = c["secret"]
			}
		}
		if secret == nil || secret != r.Form.Get("client_secret") {
			w.WriteHeader(http.StatusUnauthorized)
			json.NewEncoder(w).Encode(map[string]string{"error": "unauthorized_client"})
			return
		}
		if u == nil || u["enabled"] != true || f.password[r.Form.Get("username")] != r.Form.Get("password") {
			w.WriteHeader(http.StatusUnauthorized)
			json.NewEncoder(w).Encode(map[string]string{"error": "invalid_grant", "error_description": "Invalid user credentials"})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"access_token": "token-of-" + r.Form.Get("username"), "expires_in": 300})
	case r.Header.Get("Authorization") != "Bearer admin":
		w.WriteHeader(http.StatusUnauthorized)
	case p == "/admin/realms/garrison/clients" && r.Method == http.MethodGet:
		out := []map[string]any{}
		for id, c := range f.clients {
			if c["clientId"] == r.URL.Query().Get("clientId") {
				out = append(out, map[string]any{"id": id})
			}
		}
		json.NewEncoder(w).Encode(out)
	case p == "/admin/realms/garrison/clients" && r.Method == http.MethodPost:
		f.clients["c1"] = body
		w.WriteHeader(http.StatusCreated)
	case p == "/admin/realms/garrison/clients/c1" && r.Method == http.MethodPut:
		f.clients["c1"] = body
		w.WriteHeader(http.StatusNoContent)
	case p == "/admin/realms/garrison/clients/c1/protocol-mappers/models" && r.Method == http.MethodGet:
		out := []map[string]string{}
		for _, m := range f.mappers {
			out = append(out, map[string]string{"name": m})
		}
		json.NewEncoder(w).Encode(out)
	case p == "/admin/realms/garrison/clients/c1/protocol-mappers/models":
		if cfg, _ := body["config"].(map[string]any); cfg["included.client.audience"] != "sso" {
			f.t.Errorf("mapper %v", body)
		}
		f.mappers = append(f.mappers, body["name"].(string))
		w.WriteHeader(http.StatusCreated)
	case p == "/admin/realms/garrison/users" && r.Method == http.MethodGet:
		out := []map[string]any{}
		if u := f.users[r.URL.Query().Get("username")]; u != nil && r.URL.Query().Get("exact") == "true" {
			out = append(out, map[string]any{"id": "id-" + u["username"].(string), "username": u["username"], "email": u["email"], "enabled": u["enabled"]})
		}
		json.NewEncoder(w).Encode(out)
	case p == "/admin/realms/garrison/users" && r.Method == http.MethodPost:
		name := body["username"].(string)
		creds := body["credentials"].([]any)[0].(map[string]any)
		f.password[name] = creds["value"].(string)
		delete(body, "credentials")
		f.users[name] = body
		w.WriteHeader(http.StatusCreated)
	case strings.HasPrefix(p, "/admin/realms/garrison/users/id-"):
		name, reset := strings.CutSuffix(strings.TrimPrefix(p, "/admin/realms/garrison/users/id-"), "/reset-password")
		if reset {
			f.password[name] = body["value"].(string)
		} else {
			for _, k := range []string{"firstName", "lastName", "email"} {
				if body[k] == nil {
					f.t.Errorf("an update without %s would remove it: %v", k, body)
				}
			}
			f.users[name] = body
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		f.t.Errorf("unexpected %s %s", r.Method, p)
		http.NotFound(w, r)
	}
}

// An agent's account is made with its first token, through the client
// agents, whose tokens name sso; a second token comes from the cache.
func TestAnAgentSignsInWithItsOwnAccount(t *testing.T) {
	f, srv := newFakeKeycloak(t)
	k := &Keycloak{URL: srv.URL, User: "root", Password: "pw"}
	ctx := context.Background()
	tok, err := k.Token(ctx, "agent-todo-media", "todo/media")
	if err != nil || tok != "token-of-agent-todo-media" {
		t.Fatalf("token %q %v", tok, err)
	}
	c := f.clients["c1"]
	if c["clientId"] != "agents" || c["standardFlowEnabled"] != false || c["directAccessGrantsEnabled"] != true || c["publicClient"] != false {
		t.Errorf("client %v", c)
	}
	if len(f.mappers) != 1 || f.mappers[0] != "audience-sso" {
		t.Errorf("mappers %v", f.mappers)
	}
	u := f.users["agent-todo-media"]
	if u["enabled"] != true || u["emailVerified"] != true || u["lastName"] != "todo media" || strings.Contains(u["lastName"].(string), "/") {
		t.Errorf("user %v", u)
	}
	if f.password["agent-todo-media"] == "" || f.password["agent-todo-media"] == "pw" {
		t.Errorf("password %q", f.password["agent-todo-media"])
	}
	n := len(f.calls)
	if tok, err := k.Token(ctx, "agent-todo-media", "todo/media"); err != nil || tok != "token-of-agent-todo-media" || len(f.calls) != n {
		t.Errorf("second token %q %v, %d calls more", tok, err, len(f.calls)-n)
	}

	// Another gateway, with the same admin password, signs in as it too.
	other := &Keycloak{URL: srv.URL, User: "root", Password: "pw"}
	if tok, err := other.Token(ctx, "agent-todo-media", "todo/media"); err != nil || tok != "token-of-agent-todo-media" {
		t.Errorf("after a restart: %q %v", tok, err)
	}
}

// A password someone changed in Keycloak is set back.
func TestAChangedPasswordIsSetBack(t *testing.T) {
	f, srv := newFakeKeycloak(t)
	k := &Keycloak{URL: srv.URL, User: "root", Password: "pw"}
	ctx := context.Background()
	if err := k.Keep(ctx, "agent-todo-media", "todo/media", true); err != nil {
		t.Fatal(err)
	}
	f.password["agent-todo-media"] = "changed"
	if tok, err := k.Token(ctx, "agent-todo-media", "todo/media"); err != nil || tok != "token-of-agent-todo-media" {
		t.Fatalf("token %q %v", tok, err)
	}
}

// A locked agent gets no token, and its account is disabled, the whole
// profile sent along.
func TestALockedAgentCannotSignIn(t *testing.T) {
	f, srv := newFakeKeycloak(t)
	k := &Keycloak{URL: srv.URL, User: "root", Password: "pw"}
	ctx := context.Background()
	if _, err := k.Token(ctx, "agent-todo-media", "todo/media"); err != nil {
		t.Fatal(err)
	}
	if err := k.Keep(ctx, "agent-todo-media", "todo/media", false); err != nil {
		t.Fatal(err)
	}
	if f.users["agent-todo-media"]["enabled"] != false {
		t.Errorf("user %v", f.users["agent-todo-media"])
	}
	if _, err := k.Token(ctx, "agent-todo-media", "todo/media"); err == nil || !strings.Contains(err.Error(), "locked") {
		t.Errorf("token of a locked agent: %v", err)
	}
	// Nothing is made for an agent that is only ever locked.
	if err := k.Keep(ctx, "agent-todo-gone", "todo/gone", false); err != nil || f.users["agent-todo-gone"] != nil {
		t.Errorf("made %v %v", f.users["agent-todo-gone"], err)
	}
}

// A run without a person browses with the agent's token, and without
// Keycloak it may not.
func TestARunWithoutAPersonSignsInAsTheAgent(t *testing.T) {
	_, srv := newFakeKeycloak(t)
	agent := &agents.Agent{Namespace: "todo", Name: "media", Tools: []string{"browse", "screenshot"}}
	routine := Person{Name: AgentAccount(agent), Unattended: true, Owner: "bob"}
	tools := &Tools{Keycloak: &Keycloak{URL: srv.URL, User: "root", Password: "pw"}}
	if tok, err := tools.signIn(context.Background(), routine, agent); err != nil || tok != "token-of-agent-todo-media" {
		t.Errorf("routine: %q %v", tok, err)
	}
	if tok, err := tools.signIn(context.Background(), Person{Name: "bob", Token: "bobs"}, agent); err != nil || tok != "bobs" {
		t.Errorf("person: %q %v", tok, err)
	}
	none := &Tools{Browser: &Browser{}}
	if _, err := none.Call(context.Background(), routine, agent, "browse", json.RawMessage(`{"url":"https://todo.example.test/"}`)); err == nil || !strings.Contains(err.Error(), "gives agents none in Keycloak") {
		t.Errorf("without Keycloak: %v", err)
	}
}

// A person in a roster who never opened Gitea has no account there yet; the
// group sync finds their email in Keycloak, so the namespace's channel shows
// for them in Open WebUI anyway.
func TestRosterEmailBeforeGitea(t *testing.T) {
	kc, ksrv := newFakeKeycloak(t)
	kc.users["carol"] = map[string]any{"username": "carol", "email": "carol@example.com", "enabled": true}
	gsrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/users/alice" {
			io.WriteString(w, `{"email":"alice@example.com"}`)
			return
		}
		http.NotFound(w, r)
	}))
	defer gsrv.Close()
	o := &OpenWebUI{Gitea: &Gitea{URL: gsrv.URL, User: "admin", Password: "pw"}}
	if got := o.email(context.Background(), "carol"); got != "" {
		t.Errorf("without Keycloak: %q", got)
	}
	o.Keycloak = &Keycloak{URL: ksrv.URL, User: "root", Password: "pw"}
	for name, want := range map[string]string{"alice": "alice@example.com", "carol": "carol@example.com", "nobody": ""} {
		if got := o.email(context.Background(), name); got != want {
			t.Errorf("%s: %q, want %q", name, got, want)
		}
	}
}
