package gitops

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"garrison/control-plane/internal/events"
)

// plainKeys "decrypts" a file "ENC[value]" of garrison.
type plainKeys struct{ calls int }

func (p *plainKeys) Decrypt(_ context.Context, ns string, data []byte) (string, error) {
	p.calls++
	if ns != "garrison" {
		return "", os.ErrPermission
	}
	return strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(string(data)), "ENC["), "]"), nil
}

func TestSyncLLMKeys(t *testing.T) {
	ctx := context.Background()
	cfg, dir, work := t.TempDir(), t.TempDir(), t.TempDir()
	state := filepath.Join(work, llmKeysState)
	os.MkdirAll(filepath.Join(cfg, "secrets"), 0o755)
	put := func(name, value string) {
		os.WriteFile(filepath.Join(cfg, "secrets", name+".yaml"), []byte("ENC["+value+"]\n"), 0o644)
	}
	var feed []string
	record := func(e events.Event) { feed = append(feed, e.Title) }
	keys := &plainKeys{}

	put("llm-openrouter", "sk-or-1")
	put("api-token", "not a provider's") // another secret of garrison: never a key file
	os.WriteFile(filepath.Join(dir, "local"), []byte("by hand"), 0o600)
	if err := syncLLMKeys(ctx, keys, cfg, dir, state, record); err != nil {
		t.Fatal(err)
	}
	read := func(p string) string { b, _ := os.ReadFile(filepath.Join(dir, p)); return string(b) }
	if read("openrouter") != "sk-or-1" {
		t.Errorf("openrouter: %q", read("openrouter"))
	}
	if fi, _ := os.Stat(filepath.Join(dir, "openrouter")); fi == nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("mode: %v", fi)
	}
	if _, err := os.Stat(filepath.Join(dir, "api-token")); err == nil {
		t.Error("api-token became a key file")
	}
	if len(feed) != 1 || !strings.Contains(feed[0], "openrouter") || strings.Contains(feed[0], "sk-or") {
		t.Errorf("feed: %v", feed)
	}

	// Unchanged: nothing written, nothing told.
	before, _ := os.Stat(filepath.Join(dir, "openrouter"))
	syncLLMKeys(ctx, keys, cfg, dir, state, record)
	after, _ := os.Stat(filepath.Join(dir, "openrouter"))
	if len(feed) != 1 || !before.ModTime().Equal(after.ModTime()) {
		t.Errorf("rewritten: %v", feed)
	}

	// A new value is written; a stored key replaces one put there by hand.
	put("llm-openrouter", "sk-or-2")
	put("llm-local", "stored")
	syncLLMKeys(ctx, keys, cfg, dir, state, record)
	if read("openrouter") != "sk-or-2" || read("local") != "stored" {
		t.Errorf("%q %q", read("openrouter"), read("local"))
	}

	// Its secret gone, the deployer's file goes; one put by hand stays.
	os.WriteFile(filepath.Join(dir, "manual"), []byte("by hand"), 0o600)
	os.Remove(filepath.Join(cfg, "secrets", "llm-openrouter.yaml"))
	os.Remove(filepath.Join(cfg, "secrets", "llm-local.yaml"))
	if err := syncLLMKeys(ctx, keys, cfg, dir, state, record); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"openrouter", "local"} {
		if _, err := os.Stat(filepath.Join(dir, p)); err == nil {
			t.Errorf("%s stays", p)
		}
	}
	if read("manual") != "by hand" {
		t.Error("a key put by hand went")
	}

	// A file changed by hand after the deployer wrote it is left alone.
	put("llm-groq", "gsk-1")
	syncLLMKeys(ctx, keys, cfg, dir, state, record)
	os.WriteFile(filepath.Join(dir, "groq"), []byte("changed by hand"), 0o600)
	os.Remove(filepath.Join(cfg, "secrets", "llm-groq.yaml"))
	syncLLMKeys(ctx, keys, cfg, dir, state, record)
	if read("groq") != "changed by hand" {
		t.Error("a key changed by hand went")
	}
}
