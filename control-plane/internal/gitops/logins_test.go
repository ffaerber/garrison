package gitops

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// unitKeys "decrypts" "ENC[<unit>:value]" with that unit's key only.
type unitKeys struct{}

func (unitKeys) Decrypt(_ context.Context, ns string, data []byte) (string, error) {
	v := strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(string(data)), "ENC["), "]")
	unit, value, _ := strings.Cut(v, ":")
	if unit != ns {
		return "", os.ErrPermission
	}
	return value, nil
}

// The units' logins land in dir/<unit>/<name>.json, root only; only
// logins, each with its own unit's key, and they go when their secret does.
func TestSyncLogins(t *testing.T) {
	ctx := context.Background()
	todo, lab, dir := t.TempDir(), t.TempDir(), t.TempDir()
	put := func(cfg, file, data string) {
		os.MkdirAll(filepath.Join(cfg, "secrets"), 0o755)
		os.WriteFile(filepath.Join(cfg, "secrets", file), []byte(data), 0o644)
	}
	shop := `{"site":"https://shop.example.com","username":"u","password":"p"}`
	put(todo, "login-shop.yaml", "ENC[todo:"+shop+"]")
	put(todo, "db-password.yaml", "ENC[todo:hunter2]")   // not a login
	put(todo, "login-broken.yaml", "ENC[todo:not json]") // left out, said
	put(lab, "login-stolen.yaml", "ENC[todo:"+shop+"]")  // another unit's key
	err := syncLogins(ctx, unitKeys{}, map[string]string{"todo": todo, "lab": lab}, dir)
	if err == nil || !strings.Contains(err.Error(), "login-broken of todo") || !strings.Contains(err.Error(), "login-stolen of lab") {
		t.Errorf("errors: %v", err)
	}
	got, _ := filepath.Glob(filepath.Join(dir, "*", "*"))
	if len(got) != 1 || got[0] != filepath.Join(dir, "todo", "shop.json") {
		t.Fatalf("files: %v", got)
	}
	if data, _ := os.ReadFile(got[0]); string(data) != shop {
		t.Errorf("shop: %s", data)
	}
	if fi, _ := os.Stat(got[0]); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", fi.Mode())
	}
	os.Remove(filepath.Join(todo, "secrets", "login-shop.yaml"))
	syncLogins(ctx, unitKeys{}, map[string]string{"todo": todo}, dir)
	if got, _ := filepath.Glob(filepath.Join(dir, "*", "*")); len(got) != 0 {
		t.Errorf("a gone login stays: %v", got)
	}
}
