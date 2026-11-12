package gitops

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

const todoStack = `stack: todo
namespace: homepage
entrypoint: web
services:
  web: { image: "{{registry}}/homepage/todo:{{commit}}", port: 8080 }
  db: { image: postgres:17-alpine }
environments:
  main: { private: true }
  dev: { private: true }
`

// fakeApps plays Gitea (API and git) and Docker for app namespaces.
type fakeApps struct {
	mu       sync.Mutex
	t        *testing.T
	repos    map[string][]string          // org -> repos
	files    map[string]map[string]string // org/repo@branch -> file -> content
	commits  map[string]string            // org/repo@branch -> commit
	packages map[string]bool              // owner/name/tag
	calls    []string
}

func (f *fakeApps) api(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := strings.TrimPrefix(r.URL.Path, "/api/v1")
	switch {
	case p == "/orgs":
		var out []map[string]string
		if r.URL.Query().Get("page") == "1" {
			for org := range f.repos {
				out = append(out, map[string]string{"username": org})
			}
			out = append(out, map[string]string{"username": "system"})
		}
		json.NewEncoder(w).Encode(out)
	case strings.HasPrefix(p, "/orgs/") && strings.HasSuffix(p, "/repos"):
		org := strings.Split(p, "/")[2]
		var out []map[string]any
		if r.URL.Query().Get("page") == "1" {
			for _, name := range f.repos[org] {
				out = append(out, map[string]any{"name": name, "default_branch": "main"})
			}
		}
		json.NewEncoder(w).Encode(out)
	case strings.HasSuffix(p, "/branches"):
		parts := strings.Split(p, "/")
		var out []map[string]string
		for k := range f.commits {
			if strings.HasPrefix(k, parts[2]+"/"+parts[3]+"@") {
				out = append(out, map[string]string{"name": strings.SplitN(k, "@", 2)[1]})
			}
		}
		json.NewEncoder(w).Encode(out)
	case strings.HasPrefix(p, "/packages/"):
		parts := strings.Split(strings.TrimPrefix(p, "/packages/"), "/")
		if !f.packages[parts[0]+"/"+parts[2]+"/"+parts[3]] {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte("{}"))
	case strings.HasPrefix(p, "/repos/"):
		parts := strings.Split(p, "/")
		if !slices.Contains(f.repos[parts[2]], parts[3]) {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(`{"name":"x"}`))
	default:
		http.NotFound(w, r)
	}
}

// branchOf maps a checkout dir .../apps/<org>/<repo>/@<branch> to org/repo@branch.
func branchOf(dir string) string {
	branch := strings.TrimPrefix(filepath.Base(dir), "@")
	repo := filepath.Base(filepath.Dir(dir))
	org := filepath.Base(filepath.Dir(filepath.Dir(dir)))
	return org + "/" + repo + "@" + branch
}

func (f *fakeApps) run(_ context.Context, _ []string, _ []byte, name string, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	call := name + " " + strings.Join(args, " ")
	f.calls = append(f.calls, call)
	dirArg := func() string {
		if i := slices.Index(args, "-C"); i >= 0 {
			return args[i+1]
		}
		return args[len(args)-1]
	}
	switch {
	case name == "git" && (slices.Contains(args, "clone") || slices.Contains(args, "reset")):
		dir := dirArg()
		os.RemoveAll(dir)
		os.MkdirAll(filepath.Join(dir, ".git"), 0o755)
		for file, content := range f.files[branchOf(dir)] {
			os.WriteFile(filepath.Join(dir, file), []byte(content), 0o644)
		}
	case name == "git" && slices.Contains(args, "rev-parse"):
		return []byte(f.commits[branchOf(dirArg())] + "\n"), nil
	}
	return nil, nil
}

func (f *fakeApps) deployed() []string {
	var out []string
	for _, c := range f.calls {
		if strings.HasPrefix(c, "docker stack deploy") {
			out = append(out, c[strings.LastIndex(c, " ")+1:])
		}
	}
	return out
}

func setupApps(t *testing.T) (*Config, *fakeApps) {
	f := &fakeApps{t: t,
		repos: map[string][]string{"homepage": {"todo", "notes"}},
		files: map[string]map[string]string{
			"homepage/todo@main":  {"stack.yaml": todoStack},
			"homepage/todo@dev":   {"stack.yaml": todoStack},
			"homepage/notes@main": {"README.md": "no stack here"},
		},
		commits: map[string]string{
			"homepage/todo@main":  "aaaaaaaaaaaa1111",
			"homepage/todo@dev":   "bbbbbbbbbbbb2222",
			"homepage/notes@main": "cccccccccccc3333",
		},
		packages: map[string]bool{"homepage/todo/aaaaaaaaaaaa": true},
	}
	srv := httptest.NewServer(http.HandlerFunc(f.api))
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "repos", "platform"), 0o755)
	os.WriteFile(filepath.Join(dir, "repos", "platform", "reserved-hostnames.yaml"), []byte("reserved: [grafana]\n"), 0o644)
	c := &Config{Domain: "example.com", GiteaURL: srv.URL, User: "admin", Org: "system", WorkDir: dir,
		Run: f.run, HTTP: srv.Client(), password: "pw", waiting: map[string]string{}}
	return c, f
}

func TestAppsDeployOnceTheirImageExists(t *testing.T) {
	c, f := setupApps(t)
	ctx := context.Background()
	apps, errs := c.syncApps(ctx, false, map[string]App{})
	if len(errs) != 0 {
		t.Fatal(errs)
	}
	// main's image exists; dev's commit has none yet; notes has no stack.yaml.
	if got := strings.Join(f.deployed(), " "); got != "todo" {
		t.Errorf("deployed %q, want todo", got)
	}
	if a := apps["homepage/todo@main"]; a.Stack != "todo" || a.Commit != "aaaaaaaaaaaa1111" {
		t.Errorf("apps = %+v", apps)
	}
	if _, ok := apps["homepage/todo@dev"]; ok {
		t.Error("dev deployed without its image")
	}
	// The image's placeholders were filled in for the commit.
	for _, call := range f.calls {
		if strings.HasPrefix(call, "docker stack deploy") && !strings.Contains(call, "--with-registry-auth") {
			t.Errorf("deploy without registry credentials: %s", call)
		}
	}

	// The pipeline pushes dev's image: dev is deployed as dev-todo; main is not redeployed.
	f.packages["homepage/todo/bbbbbbbbbbbb"] = true
	f.calls = nil
	apps, _ = c.syncApps(ctx, false, apps)
	if got := strings.Join(f.deployed(), " "); got != "dev-todo" {
		t.Errorf("deployed %q, want only dev-todo", got)
	}

	// A new commit on main without an image: main stays where it was.
	f.commits["homepage/todo@main"] = "dddddddddddd4444"
	f.calls = nil
	apps, _ = c.syncApps(ctx, false, apps)
	if len(f.deployed()) != 0 || apps["homepage/todo@main"].Commit != "aaaaaaaaaaaa1111" {
		t.Errorf("main moved without an image: %v %+v", f.deployed(), apps["homepage/todo@main"])
	}

	// The branch dev is deleted: dev-todo is taken down, main stays.
	delete(f.commits, "homepage/todo@dev")
	f.calls = nil
	apps, _ = c.syncApps(ctx, false, apps)
	if !slices.Contains(f.calls, "docker stack rm dev-todo") || slices.Contains(f.calls, "docker stack rm todo") {
		t.Errorf("calls = %v", f.calls)
	}
	if _, ok := apps["homepage/todo@dev"]; ok {
		t.Error("dev still recorded")
	}

	// The repo is deleted: todo is taken down and its hostnames are free again.
	f.repos["homepage"] = []string{"notes"}
	f.calls = nil
	apps, _ = c.syncApps(ctx, false, apps)
	if !slices.Contains(f.calls, "docker stack rm todo") || len(apps) != 0 {
		t.Errorf("calls = %v, apps = %v", f.calls, apps)
	}
	data, _ := os.ReadFile(c.claimsFile())
	if strings.Contains(string(data), `"todo"`) {
		t.Errorf("claims still hold todo: %s", data)
	}
}

func TestNoAppIsDeployedWithoutRegistryCredentials(t *testing.T) {
	c, f := setupApps(t)
	failLogin := true
	run := c.Run
	c.Run = func(ctx context.Context, env []string, stdin []byte, name string, args ...string) ([]byte, error) {
		if name == "docker" && len(args) > 0 && args[0] == "login" && failLogin {
			return nil, os.ErrDeadlineExceeded
		}
		return run(ctx, env, stdin, name, args...)
	}
	apps, errs := c.syncApps(context.Background(), false, map[string]App{})
	if len(errs) == 0 || len(f.deployed()) != 0 || len(apps) != 0 {
		t.Errorf("deployed without credentials: errs %v, deployed %v", errs, f.deployed())
	}
	// Gitea is up now: the next sync signs in and deploys.
	failLogin = false
	apps, errs = c.syncApps(context.Background(), false, apps)
	if len(errs) != 0 || strings.Join(f.deployed(), " ") != "todo" {
		t.Errorf("after signing in: errs %v, deployed %v", errs, f.deployed())
	}
	if !slices.Contains(f.calls, "docker login git.example.com --username admin --password-stdin") {
		t.Errorf("calls = %v", f.calls)
	}
}

func TestAppsThatBreakTheRulesAreNotDeployed(t *testing.T) {
	for name, stack := range map[string]string{
		"stack named unlike its repo": strings.Replace(todoStack, "stack: todo", "stack: other", 1),
		"stack of another namespace":  strings.Replace(todoStack, "namespace: homepage", "namespace: system", 1),
		"reserved hostname":           strings.Replace(todoStack, "stack: todo", "stack: grafana", 1),
		"not a valid stack":           "stack: todo\n",
	} {
		t.Run(name, func(t *testing.T) {
			c, f := setupApps(t)
			if name == "reserved hostname" {
				f.repos["homepage"] = []string{"grafana"}
				f.files["homepage/grafana@main"] = map[string]string{"stack.yaml": stack}
				f.commits["homepage/grafana@main"] = "eeee"
				f.packages["homepage/todo/eeee"] = true
			} else {
				f.files["homepage/todo@main"] = map[string]string{"stack.yaml": stack}
			}
			apps, errs := c.syncApps(context.Background(), false, map[string]App{})
			if len(errs) == 0 {
				t.Error("no error reported")
			}
			if len(f.deployed()) != 0 || len(apps) != 0 {
				t.Errorf("deployed %v, apps %v", f.deployed(), apps)
			}
		})
	}
}

func TestAHostnameIsFirstComeFirstServed(t *testing.T) {
	c, f := setupApps(t)
	ctx := context.Background()
	apps, _ := c.syncApps(ctx, false, map[string]App{})
	// Another namespace's todo would take the same hostnames.
	f.repos["other"] = []string{"todo"}
	f.files["other/todo@main"] = map[string]string{"stack.yaml": strings.Replace(todoStack, "namespace: homepage", "namespace: other", 1)}
	f.commits["other/todo@main"] = "ffff"
	f.calls = nil
	apps, errs := c.syncApps(ctx, false, apps)
	if len(errs) == 0 || len(f.deployed()) != 0 {
		t.Errorf("the clash was not refused: errs %v, deployed %v", errs, f.deployed())
	}
	if _, ok := apps["homepage/todo@main"]; !ok {
		t.Error("the first app lost its deployment")
	}
}
