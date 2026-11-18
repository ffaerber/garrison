package gitops

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"garrison/control-plane/internal/events"
	"garrison/control-plane/internal/models"
	"garrison/control-plane/internal/secrets"
)

// llmKeysState is the file, in WorkDir, of the key files the deployer wrote:
// provider -> sha256 of what it wrote. A key file not in it was put there by
// hand or by the installer, and is never removed.
const llmKeysState = "llm-keys.json"

// Decrypter reads a namespace's secret file (secrets.Keys).
type Decrypter interface {
	Decrypt(ctx context.Context, ns string, data []byte) (string, error)
}

// syncLLMKeys writes the providers' API keys that garrison's config keeps,
// secrets/llm-<provider>.yaml in its checkout cfgDir, to dir/<provider>,
// where the agent service reads them, root only. It writes a file only when
// its value changed, and removes the files it wrote whose secret is gone.
// A stored key replaces a file of the same name put there by hand.
func syncLLMKeys(ctx context.Context, keys Decrypter, cfgDir, dir, stateFile string, record func(events.Event)) error {
	written := map[string]string{}
	if data, err := os.ReadFile(stateFile); err == nil {
		if err := json.Unmarshal(data, &written); err != nil {
			return fmt.Errorf("%s: %w", stateFile, err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	stored, _ := filepath.Glob(filepath.Join(cfgDir, secrets.Dir, "llm-*.yaml"))
	want := map[string][]byte{}
	for _, f := range stored {
		name, ok := secrets.NameOf(secrets.Dir + "/" + filepath.Base(f))
		if !ok {
			continue
		}
		provider, ok := models.ProviderOf(name)
		if !ok {
			continue
		}
		data, err := os.ReadFile(f)
		if err != nil {
			return err
		}
		want[provider] = data
	}
	var errs []error
	changed := false
	for _, provider := range slices.Sorted(maps.Keys(want)) {
		value, err := keys.Decrypt(ctx, models.Namespace, want[provider])
		if err != nil {
			errs = append(errs, fmt.Errorf("the key of the LLM provider %s: %w", provider, err))
			continue
		}
		value = strings.TrimSpace(value)
		sum := hash(value)
		path := filepath.Join(dir, provider)
		if have, err := os.ReadFile(path); err == nil && string(have) == value {
			if written[provider] != sum {
				written[provider], changed = sum, true
			}
			continue
		}
		if err := writeKey(path, value); err != nil {
			errs = append(errs, err)
			continue
		}
		written[provider], changed = sum, true
		log.Printf("llm keys: wrote the key of %s", provider)
		record(events.Event{Kind: events.KindSecret, Status: events.StatusOK, Namespace: models.Namespace,
			Title: fmt.Sprintf("the API key of the LLM provider %s is in place for the agents", provider)})
	}
	for _, provider := range slices.Sorted(maps.Keys(written)) {
		if _, ok := want[provider]; ok {
			continue
		}
		path := filepath.Join(dir, provider)
		// Only what it wrote: a file changed by hand since is left alone.
		if have, err := os.ReadFile(path); err == nil && hash(string(have)) == written[provider] {
			if err := os.Remove(path); err != nil {
				errs = append(errs, err)
				continue
			}
			log.Printf("llm keys: removed the key of %s", provider)
			record(events.Event{Kind: events.KindSecret, Status: events.StatusOK, Namespace: models.Namespace,
				Title: fmt.Sprintf("the API key of the LLM provider %s is removed: its secret is gone", provider)})
		}
		delete(written, provider)
		changed = true
	}
	if changed {
		data, _ := json.MarshalIndent(written, "", "  ")
		if err := os.WriteFile(stateFile, append(data, '\n'), 0o600); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// writeKey writes a key file root only, in one step.
func writeKey(path, value string) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".key-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.WriteString(value); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func hash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
