package gateway

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"garrison/control-plane/internal/agents"
)

const testProviders = `default: ollama-cloud/gpt-oss:120b
providers:
  ollama-cloud:
    url: https://ollama.com/v1
  openrouter:
    url: https://openrouter.ai/api/v1
`

const testModels = `models:
  "ollama-cloud/gpt-oss:120b": {enabled: true}
  "ollama-cloud/qwen3-coder": {enabled: false}
  "openrouter/anthropic/claude-sonnet-5.5": {enabled: true}
  "openrouter/openai/gpt-5-mini": {enabled: true, gone: 2026-11-10}
`

func TestModelChecks(t *testing.T) {
	files := map[string]string{"llm/providers.yaml": testProviders, "llm/models.yaml": testModels}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, ok := strings.CutPrefix(r.URL.Path, "/api/v1/repos/garrison/config/raw/")
		if c, found := files[path]; ok && found {
			io.WriteString(w, c)
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	g := &Gitea{URL: srv.URL, User: "admin", Password: "pw"}
	ctx := context.Background()

	list := []agents.Agent{
		{Name: "default"},
		{Name: "bare", Model: "gpt-oss:120b"},
		{Name: "routed", Model: "openrouter/anthropic/claude-sonnet-5.5"},
		{Name: "off", Model: "qwen3-coder"},
		{Name: "retired", Model: "openrouter/openai/gpt-5-mini"},
		{Name: "typo", Model: "openrouter/anthropic/claude-sonet"},
	}
	got, err := g.modelProblems(ctx, "garrison", "abc", list)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || !strings.HasPrefix(got[0], "agents/off: model ollama-cloud/qwen3-coder is not enabled") ||
		!strings.Contains(got[1], "retired") || !strings.Contains(got[2], "not a model") ||
		!strings.Contains(got[0], "the enabled ones: ollama-cloud/gpt-oss:120b, openrouter/anthropic/claude-sonnet-5.5") {
		t.Errorf("problems:\n%s", strings.Join(got, "\n"))
	}

	// The tool lists them for agents, and for the people asking them.
	out, err := (&Tools{Gitea: g}).listModels(ctx)
	if err != nil || !strings.Contains(out, "The default, for agents whose agent.yaml has model empty: ollama-cloud/gpt-oss:120b") ||
		!strings.Contains(out, "- openrouter/anthropic/claude-sonnet-5.5\n") || !strings.Contains(out, "not enabled (a root admin may enable one in llm/models.yaml): ollama-cloud/qwen3-coder") ||
		!strings.Contains(out, "Retired; agents naming one use the default: openrouter/openai/gpt-5-mini (since 2026-11-10)") {
		t.Errorf("list_models: %v\n%s", err, out)
	}

	// The agents are garrison's: a namespace's change is not refused for them.
	if got, _ := g.modelProblems(ctx, "todo", "abc", list); len(got) != 0 {
		t.Errorf("todo: %v", got)
	}

	// A broken model list refuses garrison's own change, not the namespaces'.
	files["llm/models.yaml"] = "models:\n  \"ollama-cloud/gpt-oss:120b\": {enabled: false}\n"
	if got, _ := g.modelProblems(ctx, "garrison", "abc", nil); len(got) != 1 || !strings.Contains(got[0], "must be enabled") {
		t.Errorf("garrison: %v", got)
	}
	if got, _ := g.modelProblems(ctx, "todo", "abc", list); len(got) != 0 {
		t.Errorf("todo, with garrison's list broken: %v", got)
	}

	// An installation without the files: no checks.
	clear(files)
	if got, err := g.modelProblems(ctx, "todo", "abc", list); err != nil || len(got) != 0 {
		t.Errorf("without files: %v %v", got, err)
	}
	if out, _ := (&Tools{Gitea: g}).listModels(ctx); !strings.Contains(out, "no model list yet") {
		t.Errorf("list_models without files: %s", out)
	}
}

// fakeRepo is enough of Gitea for ProposeFiles: main, branches made by the
// change-files API, and pull requests.
type fakeRepo struct {
	mu       sync.Mutex
	branches map[string]map[string]string
	prs      map[int]string // open: number to branch
	next     int
	closed   []int
}

func (f *fakeRepo) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := strings.TrimPrefix(r.URL.Path, "/api/v1/repos/garrison/config")
	ref := r.URL.Query().Get("ref")
	switch {
	case p == "/pulls" && r.Method == http.MethodGet:
		var out []map[string]any
		for n, b := range f.prs {
			out = append(out, map[string]any{"number": n, "html_url": "pr/" + b, "head": map[string]string{"ref": b}})
		}
		json.NewEncoder(w).Encode(out)
	case p == "/pulls" && r.Method == http.MethodPost:
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		f.next++
		f.prs[f.next] = body["head"]
		json.NewEncoder(w).Encode(map[string]any{"number": f.next, "html_url": "pr/" + body["head"]})
	case strings.HasPrefix(p, "/pulls/") && r.Method == http.MethodPatch:
		var n int
		json.Unmarshal([]byte(strings.TrimPrefix(p, "/pulls/")), &n)
		delete(f.prs, n)
		f.closed = append(f.closed, n)
		io.WriteString(w, "{}")
	case strings.HasPrefix(p, "/branches/") && r.Method == http.MethodDelete:
		b := strings.TrimPrefix(p, "/branches/")
		if _, ok := f.branches[b]; !ok {
			http.NotFound(w, r)
			return
		}
		delete(f.branches, b)
		w.WriteHeader(http.StatusNoContent)
	case strings.HasPrefix(p, "/raw/"):
		c, ok := f.branches[ref][strings.TrimPrefix(p, "/raw/")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		io.WriteString(w, c)
	case strings.HasPrefix(p, "/contents/") && r.Method == http.MethodGet:
		if _, ok := f.branches["main"][strings.TrimPrefix(p, "/contents/")]; !ok {
			http.NotFound(w, r)
			return
		}
		io.WriteString(w, `{"sha":"s1"}`)
	case p == "/contents" && r.Method == http.MethodPost:
		var body struct {
			NewBranch string `json:"new_branch"`
			Files     []struct {
				Path, Content, Operation string
			} `json:"files"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		b := map[string]string{}
		for k, v := range f.branches["main"] {
			b[k] = v
		}
		for _, op := range body.Files {
			data, _ := base64.StdEncoding.DecodeString(op.Content)
			b[op.Path] = string(data)
		}
		f.branches[body.NewBranch] = b
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, "{}")
	default:
		http.NotFound(w, r)
	}
}

func TestProposeFiles(t *testing.T) {
	f := &fakeRepo{branches: map[string]map[string]string{"main": {"llm/models.yaml": "old"}}, prs: map[int]string{}}
	srv := httptest.NewServer(f)
	defer srv.Close()
	g := &Gitea{URL: srv.URL, User: "admin", Password: "pw"}
	ctx := context.Background()
	// The branch of the open pull request: garrison/models-<time>.
	open := func() string {
		for _, b := range f.prs {
			return b
		}
		return ""
	}
	propose := func(content string) string {
		t.Helper()
		var files map[string][]byte
		if content != "" {
			files = map[string][]byte{"llm/models.yaml": []byte(content)}
		}
		link, err := g.ProposeFiles(ctx, "garrison", "config", "garrison/models", "Models", "body", files)
		if err != nil {
			t.Fatal(err)
		}
		return link
	}
	link := propose("new")
	first := open()
	if !strings.HasPrefix(first, "garrison/models-") || link != "pr/"+first || f.branches[first]["llm/models.yaml"] != "new" || len(f.prs) != 1 {
		t.Fatalf("first: %s %v %v", link, f.branches, f.prs)
	}
	// The same again: left as it is.
	propose("new")
	if f.next != 1 || len(f.closed) != 0 {
		t.Errorf("proposed again: %d %v", f.next, f.closed)
	}
	// Other files: the old one closed, a new one from main.
	// Other files: the old one closed and its branch gone, a new one, on a
	// branch of a new name, from main.
	time.Sleep(2 * time.Millisecond)
	propose("newer")
	second := open()
	if f.next != 2 || len(f.closed) != 1 || len(f.prs) != 1 || second == first || f.branches[first] != nil || f.branches[second]["llm/models.yaml"] != "newer" {
		t.Errorf("replaced: %d %v %v %v", f.next, f.closed, f.prs, f.branches)
	}
	// Nothing to propose: closed, branch gone.
	if link := propose(""); link != "" || len(f.prs) != 0 || f.branches[second] != nil {
		t.Errorf("closed: %s %v %v", link, f.prs, f.branches)
	}
}

// The agents a new installation starts with use only tools the gateway has.
func TestTheShippedAgentsUseRealTools(t *testing.T) {
	files := shippedFiles(agents.PlatformFiles())
	got, problems, err := agents.Load(context.Background(), files, files, agents.PlatformNamespace, ToolNames())
	if err != nil || len(problems) > 0 || len(got) < 3 {
		t.Fatalf("%d agents, %v %v", len(got), problems, err)
	}
}

// shippedFiles serves a template's files as a config repo.
type shippedFiles map[string]string

func (f shippedFiles) Read(_ context.Context, path string) (string, error) {
	c, ok := f[path]
	if !ok {
		return "", agents.ErrNotExist
	}
	return c, nil
}

func (f shippedFiles) List(_ context.Context, dir string) ([]agents.Entry, error) {
	seen := map[string]string{}
	for p := range f {
		rest, ok := strings.CutPrefix(p, dir+"/")
		if !ok {
			continue
		}
		name, _, sub := strings.Cut(rest, "/")
		seen[name] = map[bool]string{true: "dir", false: "file"}[sub]
	}
	if len(seen) == 0 {
		return nil, agents.ErrNotExist
	}
	var out []agents.Entry
	for name, typ := range seen {
		out = append(out, agents.Entry{Name: name, Type: typ})
	}
	return out, nil
}
