package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeGitea answers like Gitea for alice (may write) and bob (may only read).
type fakeGitea struct {
	mu       sync.Mutex
	files    map[string]string // repo/path -> content on main
	branches map[string]string // branch -> repo/path written there
	prs      []map[string]any
	merged   []int
	sudo     []string
}

func newFakeGitea() *fakeGitea {
	return &fakeGitea{files: map[string]string{"grafana/compose.yaml": "services: {}\n"}, branches: map[string]string{}}
}

func (f *fakeGitea) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if u, p, ok := r.BasicAuth(); !ok || u != "admin" || p != "pw" {
		http.Error(w, `{"message":"unauthorized"}`, http.StatusUnauthorized)
		return
	}
	as := r.Header.Get("Sudo")
	f.sudo = append(f.sudo, as)
	write := r.Method != http.MethodGet
	if write && as != "alice" {
		http.Error(w, `{"message":"forbidden"}`, http.StatusForbidden)
		return
	}
	p := strings.TrimPrefix(r.URL.Path, "/api/v1")
	switch {
	case p == "/orgs/system/repos":
		json.NewEncoder(w).Encode([]Repo{{Name: "grafana"}, {Name: "traefik"}})
	case strings.HasPrefix(p, "/repos/system/grafana/raw/"):
		c, ok := f.files["grafana/"+strings.TrimPrefix(p, "/repos/system/grafana/raw/")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		io.WriteString(w, c)
	case strings.HasPrefix(p, "/repos/system/grafana/contents/"):
		path := strings.TrimPrefix(p, "/repos/system/grafana/contents/")
		if r.Method == http.MethodGet {
			if _, ok := f.files["grafana/"+path]; !ok {
				http.NotFound(w, r)
				return
			}
			json.NewEncoder(w).Encode(map[string]string{"sha": "abc", "name": path, "path": path, "type": "file"})
			return
		}
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if body["branch"] != "main" || body["new_branch"] == "" {
			http.Error(w, `{"message":"must write to a new branch"}`, http.StatusBadRequest)
			return
		}
		f.branches[body["new_branch"].(string)] = "grafana/" + path
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, "{}")
	case p == "/repos/system/grafana/pulls" && r.Method == http.MethodPost:
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		body["number"] = len(f.prs) + 1
		body["html_url"] = "https://git.example.com/system/grafana/pulls/1"
		f.prs = append(f.prs, body)
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(body)
	case strings.HasSuffix(p, "/merge"):
		f.merged = append(f.merged, 1)
		io.WriteString(w, "")
	case strings.HasPrefix(p, "/repos/system/") && strings.Contains(p, "/pulls"):
		io.WriteString(w, "[]")
	default:
		http.NotFound(w, r)
	}
}

// scriptedLLM replays answers and records what it was sent.
type scriptedLLM struct {
	answers []Message
	seen    [][]Message
}

func (s *scriptedLLM) Chat(_ context.Context, msgs []Message, _ []map[string]any) (Message, error) {
	s.seen = append(s.seen, append([]Message(nil), msgs...))
	if len(s.answers) == 0 {
		return Message{}, errors.New("no more answers")
	}
	m := s.answers[0]
	s.answers = s.answers[1:]
	m.Role = "assistant"
	return m, nil
}

func callTool(id, name, args string) Message {
	var c ToolCall
	c.ID, c.Type = id, "function"
	c.Function.Name, c.Function.Arguments = name, args
	return Message{ToolCalls: []ToolCall{c}}
}

func setup(t *testing.T, llm Chatter) (*Agent, *fakeGitea) {
	t.Helper()
	fg := newFakeGitea()
	srv := httptest.NewServer(fg)
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "applied.json"), []byte(`{"stacks":{"grafana":"0123456789abcdef"}}`), 0o644)
	run := func(_ context.Context, _ []string, _ []byte, name string, args ...string) ([]byte, error) {
		if name == "docker" && len(args) > 1 && args[0] == "service" && args[1] == "ls" {
			return []byte("grafana_grafana\t1/1\tgrafana/grafana:12\nhello_web\t0/1\twhoami\n"), nil
		}
		return []byte("ok"), nil
	}
	g := &Gitea{URL: srv.URL, User: "admin", Password: "pw", Org: "system"}
	return &Agent{LLM: llm, Gitea: g, Run: run, WorkDir: dir, Domain: "example.com"}, fg
}

func TestAgentAnswersFromTools(t *testing.T) {
	llm := &scriptedLLM{answers: []Message{callTool("1", "list_stacks", "{}"), {Content: "grafana runs."}}}
	a, _ := setup(t, llm)
	reply, steps, history, err := a.Ask(context.Background(), Person{Name: "bob"}, nil, "what runs?")
	if err != nil {
		t.Fatal(err)
	}
	if reply != "grafana runs." || len(steps) != 1 || steps[0].Tool != "list_stacks" {
		t.Fatalf("reply %q, steps %+v", reply, steps)
	}
	if !strings.Contains(steps[0].Result, "grafana (0123456789ab)") || !strings.Contains(steps[0].Result, "hello (not deployed by the controller)") {
		t.Errorf("list_stacks result:\n%s", steps[0].Result)
	}
	// The tool result went back to the model, tied to its call.
	last := llm.seen[1][len(llm.seen[1])-1]
	if last.Role != "tool" || last.ToolCallID != "1" {
		t.Errorf("tool answer = %+v", last)
	}
	// History holds the turn without the system prompt.
	if history[0].Role != "user" || history[len(history)-1].Content != "grafana runs." {
		t.Errorf("history = %+v", history)
	}
	if !strings.Contains(llm.seen[0][0].Content, "bob, who is a regular user") {
		t.Errorf("system prompt does not name the person and role:\n%s", llm.seen[0][0].Content)
	}
}

func TestProposalActsAsThePerson(t *testing.T) {
	args := `{"repo":"grafana","path":"compose.yaml","content":"services: {x: {}}\n","title":"Add x"}`
	for _, tc := range []struct {
		who     string
		allowed bool
	}{{"alice", true}, {"bob", false}} {
		llm := &scriptedLLM{answers: []Message{callTool("1", "propose_change", args), {Content: "done"}}}
		a, fg := setup(t, llm)
		_, steps, _, err := a.Ask(context.Background(), Person{Name: tc.who}, nil, "add x")
		if err != nil {
			t.Fatal(err)
		}
		if steps[0].Error == tc.allowed {
			t.Errorf("%s: step = %+v", tc.who, steps[0])
		}
		if tc.allowed && len(fg.prs) != 1 {
			t.Errorf("%s: no pull request opened", tc.who)
		}
		if !tc.allowed && (len(fg.prs) != 0 || !strings.Contains(steps[0].Result, "not allowed")) {
			t.Errorf("%s: proposal not refused: %+v", tc.who, steps[0])
		}
		for _, s := range fg.sudo {
			if s != tc.who {
				t.Errorf("%s: Gitea was called as %q", tc.who, s)
			}
		}
	}
}

func TestToolArgumentsAreChecked(t *testing.T) {
	a, _ := setup(t, nil)
	for _, c := range []struct{ tool, args string }{
		{"read_file", `{"repo":"grafana","path":"../../etc/passwd"}`},
		{"read_file", `{"repo":"../x","path":"a"}`},
		{"service_logs", `{"service":"x; rm -rf /"}`},
		{"stack_status", `{"stack":"-rf"}`},
		{"nope", `{}`},
		{"read_file", `not json`},
	} {
		if _, err := a.call(context.Background(), Person{Name: "alice"}, c.tool, c.args); err == nil {
			t.Errorf("%s %s accepted", c.tool, c.args)
		}
	}
}

func TestLLMWithoutKey(t *testing.T) {
	l := &LLM{BaseURL: "http://127.0.0.1:1", Model: "m", KeyFile: filepath.Join(t.TempDir(), "missing")}
	if _, err := l.Chat(context.Background(), nil, nil); !errors.Is(err, ErrNoKey) {
		t.Errorf("err = %v, want ErrNoKey", err)
	}
}

func TestLLMSpeaksOpenAI(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer k3y" {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if body["model"] != "m" {
			http.Error(w, "bad model", http.StatusBadRequest)
			return
		}
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"hi"}}]}`)
	}))
	defer srv.Close()
	key := filepath.Join(t.TempDir(), "key")
	os.WriteFile(key, []byte("k3y\n"), 0o600)
	m, err := (&LLM{BaseURL: srv.URL + "/v1/", Model: "m", KeyFile: key}).Chat(context.Background(), []Message{{Role: "user", Content: "hi"}}, nil)
	if err != nil || m.Content != "hi" {
		t.Fatalf("m = %+v, err = %v", m, err)
	}
}

func TestAPINeedsSessionAndSameOrigin(t *testing.T) {
	a, fg := setup(t, nil)
	s := &Server{cfg: Config{PublicURL: "https://cp.example.com", Agent: a, Gitea: a.Gitea},
		sessions: map[string]*session{
			"alice": {person: Person{Name: "alice", Admin: true}, expires: time.Now().Add(time.Hour)},
			"bob":   {person: Person{Name: "bob"}, expires: time.Now().Add(time.Hour)},
			"old":   {person: Person{Name: "alice"}, expires: time.Now().Add(-time.Minute)},
		}, logins: map[string]pendingLogin{}}
	h := s.Handler()
	do := func(method, path, cookie, origin string) int {
		r := httptest.NewRequest(method, path, strings.NewReader("{}"))
		if cookie != "" {
			r.AddCookie(&http.Cookie{Name: sessionCookie, Value: cookie})
		}
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	for _, c := range []struct {
		name, method, path, cookie, origin string
		want                               int
	}{
		{"no session", "GET", "/api/stacks", "", "", 401},
		{"expired session", "GET", "/api/me", "old", "", 401},
		{"forged cookie", "GET", "/api/me", "alice-forged", "", 401},
		{"page without session", "GET", "/", "", "", 302},
		{"page with session", "GET", "/", "bob", "", 200},
		{"stacks", "GET", "/api/stacks", "bob", "", 200},
		{"approve from another site", "POST", "/api/changes/grafana/1/approve", "alice", "https://evil.example", 403},
		{"approve without origin", "POST", "/api/changes/grafana/1/approve", "alice", "", 403},
		{"bob may not approve", "POST", "/api/changes/grafana/1/approve", "bob", "https://cp.example.com", 403},
		{"alice approves", "POST", "/api/changes/grafana/1/approve", "alice", "https://cp.example.com", 200},
		{"bad repo name", "POST", "/api/changes/..%2Fx/1/approve", "alice", "https://cp.example.com", 400},
		{"unknown action", "POST", "/api/changes/grafana/1/delete", "alice", "https://cp.example.com", 400},
	} {
		if got := do(c.method, c.path, c.cookie, c.origin); got != c.want {
			t.Errorf("%s: %d, want %d", c.name, got, c.want)
		}
	}
	if len(fg.merged) != 1 {
		t.Errorf("merged %d times, want once (alice)", len(fg.merged))
	}
}
