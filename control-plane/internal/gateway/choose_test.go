package gateway

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path"
	"strings"
	"sync"
	"testing"
)

// fakeConfig is garrison/config in a fake Gitea: files on main, and what
// one proposal wrote.
type fakeConfig struct {
	mu       sync.Mutex
	files    map[string]string
	proposed map[string]string
	by       string
	title    string
	body     string
}

func (f *fakeConfig) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := r.URL.Path
	switch {
	case p == "/api/v1/users/root":
		io.WriteString(w, `{"is_admin":true}`)
	case strings.HasPrefix(p, "/api/v1/users/"):
		io.WriteString(w, `{"is_admin":false}`)
	case strings.HasPrefix(p, "/api/v1/repos/garrison/config/raw/"):
		if c, ok := f.files[strings.TrimPrefix(p, "/api/v1/repos/garrison/config/raw/")]; ok {
			io.WriteString(w, c)
			return
		}
		http.NotFound(w, r)
	case p == "/api/v1/repos/garrison/config/contents" && r.Method == http.MethodPost:
		f.by = r.Header.Get("Sudo")
		var body struct {
			Files []struct{ Path, Content string }
		}
		json.NewDecoder(r.Body).Decode(&body)
		f.proposed = map[string]string{}
		for _, x := range body.Files {
			data, _ := base64.StdEncoding.DecodeString(x.Content)
			f.proposed[x.Path] = string(data)
		}
		io.WriteString(w, "{}")
	case strings.HasPrefix(p, "/api/v1/repos/garrison/config/contents/"):
		dir := strings.TrimPrefix(p, "/api/v1/repos/garrison/config/contents/")
		if _, ok := f.files[dir]; ok {
			io.WriteString(w, `{"sha":"s1"}`)
			return
		}
		var list []map[string]string
		seen := map[string]bool{}
		for name := range f.files {
			rest, ok := strings.CutPrefix(name, dir+"/")
			if !ok {
				continue
			}
			first, _, sub := strings.Cut(rest, "/")
			if seen[first] {
				continue
			}
			seen[first] = true
			kind := "file"
			if sub {
				kind = "dir"
			}
			list = append(list, map[string]string{"name": first, "type": kind, "path": path.Join(dir, first)})
		}
		if list == nil {
			http.NotFound(w, r)
			return
		}
		json.NewEncoder(w).Encode(list)
	case p == "/api/v1/repos/garrison/config/pulls" && r.Method == http.MethodPost:
		var body struct{ Title, Body string }
		json.NewDecoder(r.Body).Decode(&body)
		f.title, f.body = body.Title, body.Body
		io.WriteString(w, `{"number":12,"html_url":"https://git/garrison/config/pulls/12"}`)
	default:
		http.NotFound(w, r)
	}
}

func TestProposeModels(t *testing.T) {
	f := &fakeConfig{files: map[string]string{
		"llm/providers.yaml": "# the providers\ndefault: \"local/qwen3.8-27b\"\nproviders:\n  local:\n    url: http://10.0.0.2:11435/v1\n  ollama-cloud:\n    url: https://ollama.com/v1\n",
		"llm/models.yaml": "models:\n  \"local/qwen3.8-27b\": {enabled: true}\n  \"ollama-cloud/glm-5.3\": {enabled: false}\n" +
			"  \"ollama-cloud/kimi-k3\": {enabled: true}\n  \"ollama-cloud/old\": {enabled: false, gone: 2026-10-01}\n",
		"agents/coder/agent.yaml":    "description: Codes\nmodel: \"\"\ntools: [read_file]\n",
		"agents/coder/prompt.md":     "You code.\n",
		"agents/operator/agent.yaml": "description: Operates\nmodel: \"ollama-cloud/kimi-k3\"     # an enabled model\ntools: all\n",
		"agents/operator/prompt.md":  "You operate.\n",
	}}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	defer srv.Close()
	g := &Gitea{URL: srv.URL, User: "admin", Password: "pw"}
	ctx := context.Background()

	// The default stays; the coder goes on GLM, which is turned on with it;
	// the operator goes back to the default.
	c, err := g.ProposeModels(ctx, "root", "", map[string]string{"coder": "ollama-cloud/glm-5.3", "operator": ""}, "")
	if err != nil {
		t.Fatal(err)
	}
	if c.Number != 12 || f.by != "root" {
		t.Errorf("pull request %d by %q", c.Number, f.by)
	}
	if got := f.proposed["agents/coder/agent.yaml"]; got != "description: Codes\nmodel: \"ollama-cloud/glm-5.3\"\ntools: [read_file]\n" {
		t.Errorf("coder:\n%s", got)
	}
	if got := f.proposed["agents/operator/agent.yaml"]; got != "description: Operates\nmodel: \"\"     # an enabled model\ntools: all\n" {
		t.Errorf("operator:\n%s", got)
	}
	if got := f.proposed["llm/models.yaml"]; !strings.Contains(got, `"ollama-cloud/glm-5.3": {enabled: true}`) || !strings.Contains(got, `"ollama-cloud/old": {enabled: false, gone: 2026-10-01}`) {
		t.Errorf("models:\n%s", got)
	}
	if _, ok := f.proposed["llm/providers.yaml"]; ok {
		t.Error("the default was not changed, but providers.yaml was")
	}
	if f.title != "Models: coder on ollama-cloud/glm-5.3, operator on the default" || !strings.Contains(f.body, "**coder** uses ollama-cloud/glm-5.3") {
		t.Errorf("%s\n%s", f.title, f.body)
	}

	// A new default: providers.yaml, and nothing else when it is on already.
	if _, err := g.ProposeModels(ctx, "root", "ollama-cloud/kimi-k3", nil, ""); err != nil {
		t.Fatal(err)
	}
	if len(f.proposed) != 1 || !strings.Contains(f.proposed["llm/providers.yaml"], "default: \"ollama-cloud/kimi-k3\"\nproviders:") ||
		!strings.HasPrefix(f.proposed["llm/providers.yaml"], "# the providers\n") {
		t.Errorf("default: %v", f.proposed)
	}

	// Refused: not a root admin, a model gone or unknown, an agent not
	// there, and nothing to change.
	if _, err := g.ProposeModels(ctx, "alice", "", map[string]string{"coder": "ollama-cloud/glm-5.3"}, ""); !errors.Is(err, ErrForbidden) {
		t.Errorf("alice: %v", err)
	}
	for _, c := range []struct {
		def    string
		choose map[string]string
	}{
		{"ollama-cloud/old", nil},
		{"", map[string]string{"coder": "ollama-cloud/nothing"}},
		{"", map[string]string{"painter": "ollama-cloud/glm-5.3"}},
		{"", map[string]string{"coder": "", "operator": "ollama-cloud/kimi-k3"}},
	} {
		if _, err := g.ProposeModels(ctx, "root", c.def, c.choose, ""); !errors.Is(err, ErrInvalid) {
			t.Errorf("%v: %v", c, err)
		}
	}
}

func TestSoleRoot(t *testing.T) {
	for _, c := range []struct {
		authors, roots []string
		want           bool
	}{
		{[]string{"admin"}, []string{"admin"}, true},
		{[]string{"admin", "admin"}, []string{"admin"}, true},
		{[]string{"admin", "alice"}, []string{"admin"}, false}, // someone else pushed to it
		{[]string{"alice"}, []string{"admin"}, false},
		{[]string{"admin"}, []string{"admin", "alice"}, false}, // alice can approve
		{nil, []string{"admin"}, false},
	} {
		if _, got := SoleRoot(c.authors, c.roots); got != c.want {
			t.Errorf("%v %v: %v", c.authors, c.roots, got)
		}
	}
}

func TestPeopleLeaveOutThePlatformAccount(t *testing.T) {
	roots := []string{"admin", "felix"}
	got := people(roots, "admin")
	if len(got) != 1 || got[0] != "felix" || len(roots) != 2 {
		t.Errorf("%v (roots %v)", got, roots)
	}
	if _, alone := SoleRoot([]string{"felix"}, got); !alone {
		t.Error("felix, the only person, does not pass his own change")
	}
	if _, alone := SoleRoot([]string{"admin"}, got); alone {
		t.Error("the platform's account passes its own change, an agent's skill")
	}
}
