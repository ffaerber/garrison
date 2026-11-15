package gitops

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"homecloud/control-plane/internal/events"
)

const todoStack = `source: todo
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
	pulls    map[string]string            // org/repo -> its open pull requests, as JSON
	props    map[string]string            // ZFS dataset -> homecloud:restored
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
	case strings.HasSuffix(p, "/pulls"):
		parts := strings.Split(p, "/")
		if js, ok := f.pulls[parts[2]+"/"+parts[3]]; ok {
			w.Write([]byte(js))
			return
		}
		w.Write([]byte("[]"))
	case strings.HasSuffix(p, "/branches"):
		parts := strings.Split(p, "/")
		if !slices.Contains(f.repos[parts[2]], parts[3]) {
			http.NotFound(w, r)
			return
		}
		// In pages of 50, by name, as Gitea caps them.
		var names []string
		for k := range f.commits {
			if strings.HasPrefix(k, parts[2]+"/"+parts[3]+"@") {
				names = append(names, k)
			}
		}
		slices.Sort(names)
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		from := min(max(page-1, 0)*50, len(names))
		out := []map[string]any{}
		for _, k := range names[from:min(from+50, len(names))] {
			out = append(out, map[string]any{"name": strings.SplitN(k, "@", 2)[1], "commit": map[string]string{"id": f.commits[k]}})
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
			os.MkdirAll(filepath.Dir(filepath.Join(dir, file)), 0o755)
			os.WriteFile(filepath.Join(dir, file), []byte(content), 0o644)
		}
	case name == "git" && slices.Contains(args, "rev-parse"):
		return []byte(f.commits[branchOf(dirArg())] + "\n"), nil
	case name == "git" && slices.Contains(args, "log"):
		// The commit that last changed a file: config's head will do.
		return []byte(f.commits[branchOf(dirArg())] + "\n"), nil
	case name == "zfs" && args[0] == "get":
		if v, ok := f.props[args[len(args)-1]]; ok {
			return []byte(v + "\n"), nil
		}
		return []byte("-\n"), nil
	case name == "zfs" && args[0] == "set":
		_, v, _ := strings.Cut(args[1], "=")
		f.props[args[len(args)-1]] = v
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
		repos: map[string][]string{"homepage": {"config", "todo", "notes"}},
		files: map[string]map[string]string{
			"homepage/config@main": {"members.yaml": "admins: [alice]\n", "stacks/todo.yaml": todoStack},
		},
		commits: map[string]string{
			"homepage/config@main": "c0c0c0c0c0c01111",
			"homepage/todo@main":   "aaaaaaaaaaaa1111",
			"homepage/todo@dev":    "bbbbbbbbbbbb2222",
			"homepage/notes@main":  "cccccccccccc3333",
		},
		packages: map[string]bool{"homepage/todo/aaaaaaaaaaaa": true},
		props:    map[string]string{},
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

	// A change to its stack file redeploys it (main back at its built commit).
	f.commits["homepage/todo@main"] = "aaaaaaaaaaaa1111"
	f.files["homepage/config@main"]["stacks/todo.yaml"] = strings.Replace(todoStack, "port: 8080 }", "port: 8080, resources: { memory: 128Mi } }", 1)
	f.commits["homepage/config@main"] = "c0c0c0c0c0c02222"
	f.calls = nil
	apps, _ = c.syncApps(ctx, false, apps)
	if got := strings.Join(f.deployed(), " "); got != "todo" {
		t.Errorf("deployed %q after its stack changed, want todo", got)
	}
	// Another change of config, not to it, does not.
	f.files["homepage/config@main"]["members.yaml"] = "admins: [alice, carol]\n"
	f.commits["homepage/config@main"] = "c0c0c0c0c0c03333"
	f.calls = nil
	apps, _ = c.syncApps(ctx, false, apps)
	if len(f.deployed()) != 0 {
		t.Errorf("redeployed %v for members.yaml", f.deployed())
	}

	// The stack file is removed: todo is taken down and its hostnames are free again.
	delete(f.files["homepage/config@main"], "stacks/todo.yaml")
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
	for name, c := range map[string]struct{ file, stack string }{
		"stack named unlike its file": {"todo", "stack: other\n" + todoStack},
		"stack of another namespace":  {"todo", "namespace: system\n" + todoStack},
		"reserved hostname":           {"grafana", todoStack},
		"not a valid stack":           {"todo", "source: todo\n"},
		"source that does not exist":  {"todo", strings.Replace(todoStack, "source: todo", "source: nothing", 1)},
	} {
		t.Run(name, func(t *testing.T) {
			cfg, f := setupApps(t)
			f.files["homepage/config@main"] = map[string]string{"stacks/" + c.file + ".yaml": c.stack}
			apps, errs := cfg.syncApps(context.Background(), false, map[string]App{})
			if len(errs) == 0 {
				t.Error("no error reported")
			}
			if len(f.deployed()) != 0 || len(apps) != 0 {
				t.Errorf("deployed %v, apps %v", f.deployed(), apps)
			}
		})
	}
}

func TestAStackWithoutSourceIsDeployedAsConfigSays(t *testing.T) {
	c, f := setupApps(t)
	ctx := context.Background()
	f.files["homepage/config@main"]["stacks/whoami.yaml"] = "entrypoint: web\nservices:\n  web: { image: traefik/whoami:v1.11, port: 80 }\nenvironments:\n  main: { private: true }\n"
	apps, errs := c.syncApps(ctx, false, map[string]App{})
	if len(errs) != 0 {
		t.Fatal(errs)
	}
	if a := apps["homepage/whoami@main"]; a.Stack != "whoami" || a.Commit != "c0c0c0c0c0c01111" || a.Source != "" {
		t.Fatalf("apps = %+v", apps)
	}
	// Config moves on without touching it: it stays.
	f.commits["homepage/config@main"] = "c0c0c0c0c0c02222"
	f.calls = nil
	apps, _ = c.syncApps(ctx, false, apps)
	if slices.Contains(f.deployed(), "whoami") {
		t.Errorf("redeployed: %v", f.deployed())
	}
}

func TestAHostnameIsFirstComeFirstServed(t *testing.T) {
	c, f := setupApps(t)
	ctx := context.Background()
	apps, _ := c.syncApps(ctx, false, map[string]App{})
	// Another namespace's todo would take the same hostnames.
	f.repos["other"] = []string{"config", "todo"}
	f.files["other/config@main"] = map[string]string{"stacks/todo.yaml": todoStack}
	f.commits["other/config@main"] = "eeee"
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

func TestPullRequestsGetPreviews(t *testing.T) {
	c, f := setupApps(t)
	ctx := context.Background()
	f.files["homepage/config@main"]["stacks/todo.yaml"] = todoStack + "  preview: { private: true }\n"
	f.packages["homepage/todo/eeeeeeeeeeee"] = true
	f.pulls = map[string]string{"homepage/todo": `[
 {"number":7,"head":{"ref":"agent/dev/bob-1","sha":"eeeeeeeeeeee5555","repo":{"full_name":"homepage/todo"}}},
 {"number":6,"head":{"ref":"feature","sha":"ffffffffffff6666","repo":{"full_name":"someone/fork"}}}]`}
	apps, errs := c.syncApps(ctx, false, map[string]App{})
	if len(errs) != 0 {
		t.Fatal(errs)
	}
	if a, ok := apps["homepage/todo@pr-7"]; !ok || a.Stack != "pr-7-todo" || a.Commit != "eeeeeeeeeeee5555" {
		t.Fatalf("apps = %+v", apps)
	}
	if _, ok := apps["homepage/todo@pr-6"]; ok {
		t.Error("a fork's pull request got a preview")
	}
	if _, ok := apps["homepage/todo@preview"]; ok {
		t.Error("the preview environment itself was deployed")
	}
	// The pull request is merged or closed: its preview goes.
	f.pulls = nil
	f.calls = nil
	apps, _ = c.syncApps(ctx, false, apps)
	if _, ok := apps["homepage/todo@pr-7"]; ok {
		t.Error("the preview stayed")
	}
	removed := false
	for _, call := range f.calls {
		removed = removed || call == "docker stack rm pr-7-todo"
	}
	if !removed {
		t.Errorf("not taken down: %v", f.calls)
	}
}

func (f *fakeApps) called(prefix string) []string {
	var out []string
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			out = append(out, c)
		}
	}
	return out
}

func TestRedeploysSnapshotFirstAndRestoresRunOnce(t *testing.T) {
	c, f := setupApps(t)
	ctx := context.Background()
	apps, errs := c.syncApps(ctx, false, map[string]App{})
	if len(errs) != 0 {
		t.Fatal(errs)
	}
	// The first deploy has no data to keep.
	if got := f.called("zfs snapshot"); len(got) != 0 {
		t.Errorf("snapshot before the first deploy: %v", got)
	}

	// A new commit: its data is snapshotted before the redeploy.
	f.commits["homepage/todo@main"] = "eeeeeeeeeeee5555"
	f.packages["homepage/todo/eeeeeeeeeeee"] = true
	f.calls = nil
	apps, _ = c.syncApps(ctx, false, apps)
	snap := f.called("zfs snapshot -r homecloud/ns/homepage/stacks/todo@deploy-")
	deploy := slices.IndexFunc(f.calls, func(s string) bool { return strings.HasPrefix(s, "docker stack deploy") })
	if len(snap) != 1 || !strings.HasSuffix(snap[0], "-eeeeeee") || deploy < slices.Index(f.calls, snap[0]) {
		t.Errorf("calls = %v", f.calls)
	}

	// A restore request in config: carried out, then todo deployed again at its commit.
	f.files["homepage/config@main"]["restores/todo.yaml"] = "stack: todo\nsnapshot: deploy-20261113-150405-aaaaaaa\n"
	f.commits["homepage/config@main"] = "c0c0c0c0c0c04444"
	f.calls = nil
	apps, errs = c.syncApps(ctx, false, apps)
	if len(errs) != 0 {
		t.Fatal(errs)
	}
	if got := f.called("zfs set homecloud:restored=c0c0c0c0c0c04444 homecloud/ns/homepage/stacks/todo"); len(got) != 1 {
		t.Errorf("not restored: %v", f.calls)
	}
	if got := strings.Join(f.deployed(), " "); got != "todo" {
		t.Errorf("deployed %q after the restore, want todo", got)
	}
	// It is done: the next sync leaves it alone.
	f.calls = nil
	c.restoreTried = nil
	c.syncApps(ctx, false, apps)
	if got := f.called("docker service scale"); len(got) != 0 {
		t.Errorf("restored twice: %v", got)
	}

	// A request for an environment that is not deployed is reported.
	f.files["homepage/config@main"]["restores/todo.yaml"] = "stack: todo\nenvironment: dev\nsnapshot: deploy-1\n"
	f.commits["homepage/config@main"] = "c0c0c0c0c0c05555"
	_, errs = c.syncApps(ctx, false, apps)
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "restores/todo.yaml: todo is not deployed in dev") {
		t.Errorf("errs = %v", errs)
	}
}

func TestTheFeedGetsDeploysWaitsAndRemovals(t *testing.T) {
	c, f := setupApps(t)
	feed := &events.Memory{}
	c.Events = feed
	ctx := context.Background()
	apps, _ := c.syncApps(ctx, false, map[string]App{})
	// A second sync changes nothing: the wait is in the feed once.
	apps, _ = c.syncApps(ctx, false, apps)
	f.packages["homepage/todo/bbbbbbbbbbbb"] = true
	apps, _ = c.syncApps(ctx, false, apps)
	delete(f.commits, "homepage/todo@dev")
	c.syncApps(ctx, false, apps)

	var got []string
	for _, e := range feed.All() {
		if e.Source != events.SourceDeployer || e.Namespace != "homepage" {
			t.Errorf("%+v", e)
		}
		got = append(got, e.Kind+" "+e.Status+" "+e.Repo+"@"+e.Commit[:min(4, len(e.Commit))])
	}
	want := "deploy ok todo@bbbb|deploy ok todo@aaaa|undeploy ok todo@" // dev waited first
	if strings.Join(got, "|") != want {
		t.Errorf("feed:\n%s\nwant\n%s", strings.Join(got, "|"), want)
	}
	// dev's wait became its deploy: the same entry, by its key.
	for _, e := range feed.All() {
		if e.Commit == "bbbbbbbbbbbb2222" && !strings.HasPrefix(e.Title, "deployed homepage/todo@dev") {
			t.Errorf("dev's entry: %+v", e)
		}
	}
}

// A repo with more branches than Gitea lists at once, say many open
// proposals, keeps its environments: their branches are found on later pages.
func TestEnvironmentsSurviveManyBranches(t *testing.T) {
	c, f := setupApps(t)
	ctx := context.Background()
	apps, errs := c.syncApps(ctx, false, map[string]App{})
	if len(errs) != 0 || apps["homepage/todo@main"].Stack != "todo" {
		t.Fatalf("apps %+v, errs %v", apps, errs)
	}
	f.mu.Lock()
	for i := range 60 {
		f.commits[fmt.Sprintf("homepage/todo@change/alice-%02d", i)] = fmt.Sprintf("%012d", i)
	}
	f.mu.Unlock()
	apps, errs = c.syncApps(ctx, false, apps)
	if len(errs) != 0 {
		t.Fatal(errs)
	}
	if _, ok := apps["homepage/todo@main"]; !ok {
		t.Errorf("main is gone: %+v", apps)
	}
	if rm := f.called("docker stack rm"); len(rm) != 0 {
		t.Errorf("took down %v", rm)
	}
}
