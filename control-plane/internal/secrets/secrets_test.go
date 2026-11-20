package secrets

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// fakeGitea: alice is todo's admin, bob a member, root a root admin.
type fakeGitea struct {
	files   map[string]string // org/repo/path on main
	commits []string
}

func (f *fakeGitea) IsAdmin(_ context.Context, p string) (bool, error) { return p == "root", nil }
func (f *fakeGitea) Role(_ context.Context, ns, p string) (string, error) {
	if ns != "todo" {
		return "", nil
	}
	return map[string]string{"alice": "admin", "bob": "member"}[p], nil
}
func (f *fakeGitea) Orgs(context.Context, string) ([]string, error) {
	return []string{"garrison", "todo", "media"}, nil
}
func (f *fakeGitea) Files(_ context.Context, org, repo, dir string) ([]string, error) {
	var out []string
	for k := range f.files {
		if name, ok := strings.CutPrefix(k, org+"/"+repo+"/"+dir+"/"); ok && !strings.Contains(name, "/") {
			out = append(out, name)
		}
	}
	return out, nil
}
func (f *fakeGitea) File(_ context.Context, org, repo, path string) (string, error) {
	return f.files[org+"/"+repo+"/"+path], nil
}
func (f *fakeGitea) Commit(_ context.Context, org, repo, message, author string, files map[string][]byte) error {
	for p, data := range files {
		if data == nil {
			delete(f.files, org+"/"+repo+"/"+p)
		} else {
			f.files[org+"/"+repo+"/"+p] = string(data)
		}
	}
	f.commits = append(f.commits, author+": "+message)
	return nil
}

var (
	pubTodo     = "age1" + strings.Repeat("q", 58)
	pubRecovery = "age1" + strings.Repeat("r", 58)
)

// fakeSOPS "encrypts" by noting the recipients; the value must come on
// stdin, never in the arguments.
func fakeSOPS(t *testing.T) Runner {
	return func(_ context.Context, _ []string, stdin []byte, name string, args ...string) ([]byte, error) {
		if name != "sops" || args[0] != "encrypt" {
			t.Fatalf("ran %s %v", name, args)
		}
		var doc map[string]string
		if err := yaml.Unmarshal(stdin, &doc); err != nil {
			t.Fatal(err)
		}
		return []byte("value: ENC[" + doc["value"] + "]\nsops:\n  age: " + args[2] + "\n"), nil
	}
}

func TestTheStore(t *testing.T) {
	ctx := context.Background()
	g := &fakeGitea{files: map[string]string{"todo/config/secrets/db-password.yaml": "x", "todo/config/secrets/README.md": "x"}}
	s := &Store{Gitea: g, Run: fakeSOPS(t), Domain: "home.test",
		Recipients: func(_ context.Context, ns string) ([]string, error) { return []string{pubTodo, pubRecovery}, nil }}

	// Who manages which unit's secrets on the Secrets page: never
	// garrison's, whose providers' keys are on the Models page.
	for _, c := range []struct {
		who  string
		want []string
	}{{"alice", []string{"todo"}}, {"bob", nil}, {"root", []string{"media", "todo"}}} {
		got, err := s.Units(ctx, c.who)
		if err != nil || !slices.Equal(got, c.want) {
			t.Errorf("%s manages %v (%v), want %v", c.who, got, err, c.want)
		}
	}

	if names, _ := s.Names(ctx, "todo"); !slices.Equal(names, []string{"db-password"}) {
		t.Errorf("names: %v", names)
	}

	// alice sets one: encrypted for both keys, with .sops.yaml beside it.
	if err := s.Set(ctx, "todo", "shop-api-key", "tok: 1\nline", "alice"); err != nil {
		t.Fatal(err)
	}
	enc := g.files["todo/config/secrets/shop-api-key.yaml"]
	if !strings.Contains(enc, "ENC[tok: 1\nline]") || !strings.Contains(enc, pubTodo+","+pubRecovery) {
		t.Errorf("file: %q", enc)
	}
	if !strings.Contains(g.files["todo/config/.sops.yaml"], "age: "+pubTodo+","+pubRecovery) {
		t.Errorf(".sops.yaml: %q", g.files["todo/config/.sops.yaml"])
	}
	if got := g.commits[len(g.commits)-1]; got != "alice: Set the secret shop-api-key, for alice" {
		t.Errorf("commit: %q", got)
	}
	// .sops.yaml is written only when it changes.
	s.Set(ctx, "todo", "shop-api-key", "tok-2", "alice")
	if len(g.commits) != 2 {
		t.Fatal(g.commits)
	}

	// Refused: bob, garrison, bad names and values.
	for _, c := range []struct {
		ns, name, value, who string
		want                 error
	}{
		{"todo", "x", "v", "bob", ErrForbidden},
		{"garrison", "x", "v", "root", ErrPlatformName},
		{"garrison", "llm-openrouter", "v", "alice", ErrForbidden},
		{"garrison", "llm-openrouter", "v", "bob", ErrForbidden},
		{"todo", "Bad_Name", "v", "alice", ErrInvalid},
		{"todo", "../members", "v", "alice", ErrInvalid},
		{"todo", strings.Repeat("a", 33), "v", "alice", ErrInvalid},
		{"todo", "x", "", "alice", ErrInvalid},
		{"todo", "x", strings.Repeat("v", MaxValue+1), "alice", ErrInvalid},
	} {
		if err := s.Set(ctx, c.ns, c.name, c.value, c.who); !errors.Is(err, c.want) {
			t.Errorf("%s sets %s/%s: %v, want %v", c.who, c.ns, c.name, err, c.want)
		}
	}

	// garrison keeps its providers' keys, set by root admins only.
	if ok, _ := s.May(ctx, "garrison", "root"); !ok {
		t.Error("root may not manage garrison's keys")
	}
	if ok, _ := s.May(ctx, "garrison", "alice"); ok {
		t.Error("alice may manage garrison's keys")
	}
	if err := s.Set(ctx, "garrison", "llm-openrouter", "sk-or-1", "root"); err != nil {
		t.Fatal(err)
	}
	if enc := g.files["garrison/config/secrets/llm-openrouter.yaml"]; !strings.Contains(enc, "ENC[sk-or-1]") {
		t.Errorf("garrison's key: %q", enc)
	}
	if err := s.Delete(ctx, "garrison", "llm-openrouter", "alice"); !errors.Is(err, ErrForbidden) {
		t.Errorf("alice deleted garrison's key: %v", err)
	}
	if err := s.Delete(ctx, "garrison", "llm-openrouter", "root"); err != nil {
		t.Fatal(err)
	}

	// Deleting: only what exists, only by an admin.
	if err := s.Delete(ctx, "todo", "db-password", "bob"); !errors.Is(err, ErrForbidden) {
		t.Errorf("bob deleted: %v", err)
	}
	if err := s.Delete(ctx, "todo", "nope", "alice"); !errors.Is(err, ErrInvalid) {
		t.Errorf("deleted a missing one: %v", err)
	}
	if err := s.Delete(ctx, "todo", "db-password", "alice"); err != nil {
		t.Fatal(err)
	}
	if _, ok := g.files["todo/config/secrets/db-password.yaml"]; ok {
		t.Error("not deleted")
	}
}

// run is apply.Exec's like, for the tests with the real tools.
func run(ctx context.Context, env []string, stdin []byte, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = append(os.Environ(), env...)
	if stdin != nil {
		cmd.Stdin = strings.NewReader(string(stdin))
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return out, errors.New(err.Error() + ": " + stderr.String())
	}
	return out, nil
}

// With sops and age installed (they are in the control plane's image): a
// unit's key is made on first use, a secret encrypted with the public
// keys alone is decrypted by the unit's key and by the recovery key,
// and not by another unit's.
func TestKeysWithSOPS(t *testing.T) {
	for _, tool := range []string{"sops", "age-keygen"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip(tool + " not installed")
		}
	}
	ctx := context.Background()
	dir := t.TempDir()
	recovery := filepath.Join(t.TempDir(), "recovery.txt")
	if _, err := run(ctx, nil, nil, "age-keygen", "-o", recovery); err != nil {
		t.Fatal(err)
	}
	pub, err := run(ctx, nil, nil, "age-keygen", "-y", recovery)
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, RecoveryFile), pub, 0o600)

	k := &Keys{Dir: dir, Run: run}
	if k.Has("todo") {
		t.Fatal("a key before any was made")
	}
	rs, err := k.Recipients(ctx, "todo")
	if err != nil || len(rs) != 2 || rs[1] != strings.TrimSpace(string(pub)) {
		t.Fatalf("recipients: %v %v", rs, err)
	}
	if again, _ := k.Recipients(ctx, "todo"); !slices.Equal(again, rs) {
		t.Fatal("the key changed")
	}
	if fi, err := os.Stat(filepath.Join(dir, "todo.txt")); err != nil || fi.Mode().Perm()&0o077 != 0 {
		t.Fatalf("key file: %v %v", fi, err)
	}

	secret := "line one\nsecret: with \"quotes\" and ünïcode"
	enc, err := Encrypt(ctx, run, rs, secret)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(enc), "secret: with") {
		t.Fatal("the value is in the file in clear")
	}
	got, err := k.Decrypt(ctx, "todo", enc)
	if err != nil || got != secret {
		t.Fatalf("decrypt: %q %v", got, err)
	}
	plain, err := run(ctx, []string{"SOPS_AGE_KEY_FILE=" + recovery}, enc, "sops", "decrypt", "--input-type", "yaml", "--output-type", "yaml", "/dev/stdin")
	if v, _ := value(plain); err != nil || v != secret {
		t.Fatalf("the recovery key did not decrypt it: %v", err)
	}
	if _, err := k.Recipients(ctx, "media"); err != nil {
		t.Fatal(err)
	}
	if _, err := k.Decrypt(ctx, "media", enc); err == nil {
		t.Fatal("media's key decrypted todo's secret")
	}
	if _, err := k.Decrypt(ctx, "nokey", enc); err == nil {
		t.Fatal("decrypted without a key")
	}

	// The deployer's handler gives the public keys of units that exist.
	h := http.NewServeMux()
	h.Handle("GET /secrets/{unit}/recipients", k.Handler(func(_ context.Context, ns string) (bool, error) { return ns == "todo" || ns == "new", nil }))
	srv := httptest.NewServer(h)
	defer srv.Close()
	remote := Remote(srv.URL)
	if got, err := remote(ctx, "todo"); err != nil || !slices.Equal(got, rs) {
		t.Errorf("remote: %v %v", got, err)
	}
	if _, err := remote(ctx, "ghost"); err == nil || k.Has("ghost") {
		t.Errorf("a key for a unit that does not exist: %v", err)
	}
	if got, err := remote(ctx, "new"); err != nil || len(got) != 2 || !k.Has("new") {
		t.Errorf("new unit: %v %v", got, err)
	}
}
