package gitops

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// fakeGitea serves the organisation's repo list and plays git and kubectl.
type fakeGitea struct {
	t      *testing.T
	repos  map[string]map[string]string // repo -> file -> content
	commit map[string]string            // repo -> HEAD
	failOn string                       // stack whose deploy fails
	calls  []string
}

func newFake(t *testing.T) *fakeGitea {
	return &fakeGitea{t: t,
		repos: map[string]map[string]string{
			"traefik":       {"kubernetes.yaml": "kind: Deployment\nmetadata: {name: t}\n"},
			"control-plane": {"kubernetes.yaml": "kind: Deployment\nmetadata: {name: c}\n", "go.mod": "module x\n"},
			"grafana":       {"kubernetes.yaml": "kind: Deployment\nmetadata: {name: g}\n"},
			"platform":      {"README.md": "contracts\n"}, // no kubernetes.yaml: not a stack
		},
		commit: map[string]string{"traefik": "t1", "control-plane": "c1", "grafana": "g1", "platform": "p1"},
	}
}

func (f *fakeGitea) api(w http.ResponseWriter, r *http.Request) {
	if u, p, ok := r.BasicAuth(); !ok || u != "admin" || p != "secret" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var list []map[string]any
	if r.URL.Query().Get("page") == "1" {
		names := make([]string, 0, len(f.repos))
		for n := range f.repos {
			names = append(names, n)
		}
		slices.Sort(names)
		for _, n := range names {
			list = append(list, map[string]any{"name": n, "default_branch": "main"})
		}
		list = append(list, map[string]any{"name": "empty", "default_branch": "main", "empty": true})
		list = append(list, map[string]any{"name": "Bad_Name", "default_branch": "main"})
	}
	json.NewEncoder(w).Encode(list)
}

func (f *fakeGitea) run(_ context.Context, _ []string, _ []byte, name string, args ...string) ([]byte, error) {
	call := name + " " + strings.Join(args, " ")
	f.calls = append(f.calls, call)
	switch {
	case name == "git" && slices.Contains(args, "clone"):
		f.write(args[len(args)-1])
	case name == "git" && slices.Contains(args, "reset"):
		f.write(args[slices.Index(args, "-C")+1])
	case name == "git" && slices.Contains(args, "rev-parse"):
		return []byte(f.commit[filepath.Base(args[slices.Index(args, "-C")+1])] + "\n"), nil
	case name == "kubectl" && args[0] == "apply":
		if stackOf(args) == f.failOn {
			return nil, errors.New("deploy failed")
		}
	}
	return nil, nil
}

func (f *fakeGitea) write(dir string) {
	os.RemoveAll(dir)
	os.MkdirAll(filepath.Join(dir, ".git"), 0o755)
	for file, content := range f.repos[filepath.Base(dir)] {
		if err := os.WriteFile(filepath.Join(dir, file), []byte(content), 0o644); err != nil {
			f.t.Fatal(err)
		}
	}
}

// stackOf is the stack a kubectl command selects.
func stackOf(args []string) string {
	for i, a := range args {
		if a == "--selector" && i+1 < len(args) {
			return strings.TrimPrefix(args[i+1], "garrison.io/stack=")
		}
	}
	return ""
}

func (f *fakeGitea) deploys() []string {
	var out []string
	for _, c := range f.calls {
		if strings.HasPrefix(c, "kubectl apply") {
			out = append(out, stackOf(strings.Fields(c)))
		}
	}
	return out
}

func (f *fakeGitea) removed(name string) bool {
	for _, c := range f.calls {
		if strings.HasPrefix(c, "kubectl delete") && stackOf(strings.Fields(c)) == name {
			return true
		}
	}
	return false
}

func setup(t *testing.T) (*Config, *fakeGitea) {
	f := newFake(t)
	srv := httptest.NewServer(http.HandlerFunc(f.api))
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	pw := filepath.Join(dir, "pw")
	os.WriteFile(pw, []byte("secret\n"), 0o600)
	c := &Config{Domain: "example.com", GiteaURL: srv.URL, User: "admin", PasswordFile: pw,
		Org: "garrison", WorkDir: filepath.Join(dir, "work"), Run: f.run}
	return c, f
}

func TestOnceDeploysEveryStackControlPlaneLast(t *testing.T) {
	c, f := setup(t)
	s, err := c.Once(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(f.deploys(), " "); got != "grafana traefik control-plane" {
		t.Errorf("deploy order = %q", got)
	}
	if got := strings.Join(s.Names(), " "); got != "control-plane grafana traefik" {
		t.Errorf("applied = %q", got)
	}
	for _, call := range f.calls {
		if strings.Contains(call, "secret") {
			t.Errorf("password on a command line: %s", call)
		}
	}
}

func TestOnlyMovedReposAreDeployed(t *testing.T) {
	c, f := setup(t)
	ctx := context.Background()
	c.Once(ctx)
	f.calls = nil
	if _, err := c.Sync(ctx, false); err != nil {
		t.Fatal(err)
	}
	if len(f.deploys()) != 0 {
		t.Errorf("unchanged repos deployed: %v", f.deploys())
	}
	f.commit["grafana"] = "g2"
	if _, err := c.Sync(ctx, false); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(f.deploys(), " "); got != "grafana" {
		t.Errorf("deployed %q, want only grafana", got)
	}
}

func TestStackIsRemovedWithItsRepo(t *testing.T) {
	c, f := setup(t)
	ctx := context.Background()
	c.Once(ctx)
	delete(f.repos, "grafana")
	s, err := c.Sync(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if !f.removed("grafana") {
		t.Error("grafana was not taken down")
	}
	if _, ok := s.Stacks["grafana"]; ok {
		t.Error("grafana still recorded")
	}
	// A repo that drops its kubernetes.yaml stops being a stack too.
	delete(f.repos["traefik"], "kubernetes.yaml")
	f.commit["traefik"] = "t2"
	c.Sync(ctx, false)
	if !f.removed("traefik") {
		t.Error("traefik was not taken down after losing kubernetes.yaml")
	}
	if f.removed("platform") {
		t.Error("a repo that never was a stack was removed")
	}
}

func TestFailedDeployIsRetriedAndHoldsBackTheControlPlane(t *testing.T) {
	c, f := setup(t)
	ctx := context.Background()
	f.failOn = "grafana"
	if _, err := c.Once(ctx); err == nil {
		t.Fatal("failed deploy reported success")
	}
	if slices.Contains(f.deploys(), "control-plane") {
		t.Error("control-plane was replaced although another deploy failed")
	}
	if s, _ := ReadState(c.WorkDir); s.Stacks["traefik"] != "t1" || s.Stacks["grafana"] != "" {
		t.Errorf("state after partial failure = %v", s.Stacks)
	}
	f.failOn = ""
	f.calls = nil
	if _, err := c.Sync(ctx, false); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(f.deploys(), " "); got != "grafana control-plane" {
		t.Errorf("retry deployed %q, want grafana then control-plane", got)
	}
}

func TestNetrcNamesTheGiteaHost(t *testing.T) {
	c, _ := setup(t)
	c.GiteaURL = "http://gitea:3000"
	c.prepare()
	if netrc, _ := os.ReadFile(filepath.Join(c.WorkDir, "home", "gitea", ".netrc")); !strings.HasPrefix(string(netrc), "machine gitea login admin ") {
		t.Errorf(".netrc = %q", netrc)
	}
}
