package agentapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"garrison/control-plane/internal/events"
	"garrison/control-plane/internal/gateway"
	"garrison/control-plane/internal/models"
)

type recorded struct {
	mu sync.Mutex
	es []events.Event
}

func (r *recorded) Record(_ context.Context, e events.Event) {
	r.mu.Lock()
	r.es = append(r.es, e)
	r.mu.Unlock()
}

func TestModelsRouteAndSeed(t *testing.T) {
	// A provider: lists its models, and says which key it got.
	var listed atomic.Value
	listed.Store(`{"data":[{"id":"small"},{"id":"large"}]}`)
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			io.WriteString(w, listed.Load().(string))
			return
		}
		http.NotFound(w, r)
	}))
	defer provider.Close()

	var mu sync.Mutex
	files := map[string]string{}
	var commits []string
	pulls := 0
	gitea := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		p := strings.TrimPrefix(r.URL.Path, "/api/v1/repos/garrison/config")
		switch {
		case strings.HasPrefix(p, "/raw/"):
			if c, ok := files[strings.TrimPrefix(p, "/raw/")]; ok {
				io.WriteString(w, c)
				return
			}
			http.NotFound(w, r)
		case p == "/pulls" && r.Method == http.MethodGet:
			io.WriteString(w, "[]")
		case p == "/pulls" && r.Method == http.MethodPost:
			pulls++
			io.WriteString(w, `{"number": 7, "html_url": "https://git.home.test/garrison/config/pulls/7"}`)
		case p == "/contents" && r.Method == http.MethodPost:
			var body struct {
				Branch, Message string
				Files           []struct{ Path, Content string }
			}
			json.NewDecoder(r.Body).Decode(&body)
			for _, f := range body.Files {
				data, _ := base64.StdEncoding.DecodeString(f.Content)
				files[f.Path] = string(data)
			}
			commits = append(commits, body.Branch+": "+body.Message)
			w.WriteHeader(http.StatusCreated)
			io.WriteString(w, "{}")
		default:
			http.NotFound(w, r)
		}
	}))
	defer gitea.Close()

	dir := t.TempDir()
	installKey := filepath.Join(dir, "llm-api-key")
	os.WriteFile(installKey, []byte("install-key"), 0o600)
	keys := filepath.Join(dir, "llm-keys")
	os.Mkdir(keys, 0o700)
	rec := &recorded{}
	m := &Models{
		Gitea: &gateway.Gitea{URL: gitea.URL, User: "admin", Password: "pw"}, KeyDir: keys,
		InstallURL: provider.URL + "/v1", InstallModel: "small", InstallKey: installKey,
		Events: rec, Domain: "home.test",
	}
	ctx := context.Background()

	// Before any files: the install's LLM.
	if llm, model, _ := m.Route(ctx, "todo/dev", ""); llm.URL != provider.URL+"/v1" || model != "small" || llm.key() != "install-key" {
		t.Fatalf("before the files: %s %s", llm.URL, model)
	}

	// The first sync writes the install's LLM and its models to main.
	if _, err := m.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if len(commits) != 1 || !strings.HasPrefix(commits[0], "main: ") ||
		!strings.Contains(files["llm/providers.yaml"], `default: "local/small"`) ||
		!strings.Contains(files["llm/models.yaml"], `"local/small": {enabled: true`) ||
		!strings.Contains(files["llm/models.yaml"], `"local/large": {enabled: false`) {
		t.Fatalf("seed: %v\n%s\n%s", commits, files["llm/providers.yaml"], files["llm/models.yaml"])
	}

	// Routed by the files now; the install's key serves its provider until
	// llm-keys has one.
	llm, model, full := m.Route(ctx, "todo/dev", "local/small")
	if llm.URL != provider.URL+"/v1" || model != "small" || full != "local/small" || llm.key() != "install-key" {
		t.Errorf("routed: %s %s %s", llm.URL, model, full)
	}
	os.WriteFile(filepath.Join(keys, "local"), []byte("own-key"), 0o600)
	if llm, _, _ := m.Route(ctx, "todo/dev", ""); llm.key() != "own-key" {
		t.Errorf("key: %q", llm.key())
	}

	// A model that is off: the default, and the feed is told once.
	for range 2 {
		if _, model, full := m.Route(ctx, "todo/dev", "large"); model != "small" || full != "local/small" {
			t.Errorf("fallback: %s %s", model, full)
		}
	}
	if len(rec.es) != 1 || rec.es[0].Kind != events.KindModel || rec.es[0].Unit != "todo" ||
		!strings.Contains(rec.es[0].Detail, "local/large is not enabled") || !strings.Contains(rec.es[0].Link, "garrison/config/src/branch/main/agents/dev/agent.yaml") {
		t.Errorf("feed: %+v", rec.es)
	}

	// Nothing changed at the provider: nothing to commit.
	if _, err := m.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if len(commits) != 1 {
		t.Errorf("committed again: %v", commits)
	}

	// Only new models: on main at once, turned off, without a pull request.
	listed.Store(`{"data":[{"id":"small"},{"id":"large"},{"id":"glm-5.3"}]}`)
	if link, err := m.Sync(ctx); err != nil || link != "" {
		t.Fatal(link, err)
	}
	if len(commits) != 2 || !strings.HasPrefix(commits[1], "main: Models, turned off: local: 1 new") || pulls != 0 ||
		!strings.Contains(files["llm/models.yaml"], `"local/glm-5.3": {enabled: false`) {
		t.Fatalf("added: %v %d\n%s", commits, pulls, files["llm/models.yaml"])
	}

	// One gone: proposed, for a person to see which agents it touches.
	listed.Store(`{"data":[{"id":"small"},{"id":"glm-5.3"}]}`)
	if link, err := m.Sync(ctx); err != nil || !strings.HasSuffix(link, "/pulls/7") {
		t.Fatal(link, err)
	}
	if pulls != 1 || len(commits) != 3 || commits[2] != "main: Models: local: 1 retired" {
		t.Errorf("gone: %v %d", commits, pulls)
	}
}

func TestOnlyAdded(t *testing.T) {
	for _, c := range []struct {
		changes []models.Change
		want    bool
	}{
		{nil, false},
		{[]models.Change{{Provider: "a", Added: []string{"a/x"}}, {Provider: "b", Unreachable: "401"}}, true},
		{[]models.Change{{Provider: "a", Added: []string{"a/x"}}, {Provider: "b", Gone: []string{"b/y"}}}, false},
		{[]models.Change{{Provider: "a", Dropped: []string{"a/z"}}}, false},
	} {
		if got := onlyAdded(c.changes); got != c.want {
			t.Errorf("%+v: %v", c.changes, got)
		}
	}
}
