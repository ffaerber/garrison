package hostnames

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"homecloud/controller/internal/stack"
)

func mk(t *testing.T, namespace, name string, envs ...string) *stack.Stack {
	t.Helper()
	y := fmt.Sprintf("stack: %s\nnamespace: %s\nentrypoint: web\nservices:\n  web: { image: a:1, port: 80 }\n  adm: { image: a:1, port: 81, public: admin }\nenvironments:\n", name, namespace)
	for _, e := range envs {
		y += "  " + e + ": {}\n"
	}
	s, err := stack.Parse([]byte(y))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestOf(t *testing.T) {
	var got []string
	for _, c := range Of(mk(t, "media", "jellyfin", "main", "dev")) {
		got = append(got, c.Hostname)
	}
	want := "dev-jellyfin-admin dev-jellyfin jellyfin-admin jellyfin"
	if strings.Join(got, " ") != want {
		t.Errorf("claims = %v, want %s", got, want)
	}
}

func TestClaim(t *testing.T) {
	r := NewRegistry([]string{"git"})
	if err := r.Claim(mk(t, "media", "jellyfin", "main", "dev")); err != nil {
		t.Fatal(err)
	}

	// Sharing a prefix is not a clash.
	if err := r.Claim(mk(t, "media", "jelly", "main")); err != nil {
		t.Errorf("prefix-sharing stack rejected: %v", err)
	}
	// Redeploying the same stack is not a clash, and drops names it no longer declares.
	if err := r.Claim(mk(t, "media", "jellyfin", "main")); err != nil {
		t.Errorf("redeploy rejected: %v", err)
	}
	if err := r.Claim(mk(t, "other", "dev-jellyfin", "main")); err != nil {
		t.Errorf("released hostname still held: %v", err)
	}

	for name, s := range map[string]*stack.Stack{
		"same name in another namespace":        mk(t, "other", "jellyfin", "main"),
		"stack named like a public service":     mk(t, "other", "jellyfin-admin", "main"),
		"environment hostname of another stack": mk(t, "media", "jellyfin", "main", "dev"),
		"reserved":                              mk(t, "other", "git", "main"),
	} {
		before := len(r.held)
		if err := r.Claim(s); err == nil {
			t.Errorf("%s: clash accepted", name)
		}
		if len(r.held) != before {
			t.Errorf("%s: rejected claim changed the registry", name)
		}
	}
}

func TestSaveLoad(t *testing.T) {
	file := filepath.Join(t.TempDir(), "state", "claims.json")
	r := NewRegistry(nil)
	if err := r.Load(file); err != nil {
		t.Fatalf("missing file: %v", err)
	}
	if err := r.Claim(mk(t, "media", "jellyfin", "main")); err != nil {
		t.Fatal(err)
	}
	if err := r.Save(file); err != nil {
		t.Fatal(err)
	}
	r2 := NewRegistry(nil)
	if err := r2.Load(file); err != nil {
		t.Fatal(err)
	}
	if err := r2.Claim(mk(t, "other", "jellyfin", "main")); err == nil {
		t.Error("clash with a loaded claim accepted")
	}
}
