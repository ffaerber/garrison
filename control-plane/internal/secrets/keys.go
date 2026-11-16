package secrets

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

// RecoveryFile holds the recovery key's public half, in the keys'
// directory; the installer writes it.
const RecoveryFile = "recovery.pub"

// namespace is what a namespace may be called (Gitea's organisations, as
// the deployer takes them).
var namespace = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,38}[a-z0-9])?$`)

// Keys are the namespaces' age keys, <Dir>/<namespace>.txt, root only. Only
// the deployer has them: it makes a namespace's key when its public half is
// first asked for, and decrypts with it.
type Keys struct {
	Dir string
	Run Runner
	mu  sync.Mutex
}

func (k *Keys) file(ns string) string { return filepath.Join(k.Dir, ns+".txt") }

// Recipients are the public keys a namespace's secrets are encrypted for:
// its own, made now if it has none, and the recovery key, if the
// installation has one.
func (k *Keys) Recipients(ctx context.Context, ns string) ([]string, error) {
	if !namespace.MatchString(ns) {
		return nil, fmt.Errorf("invalid namespace %q", ns)
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	file := k.file(ns)
	if _, err := os.Stat(file); errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(k.Dir, 0o700); err != nil {
			return nil, err
		}
		// age-keygen writes the key to a new file, readable by root only.
		if _, err := k.Run(ctx, nil, nil, "age-keygen", "-o", file); err != nil {
			return nil, fmt.Errorf("make %s's key: %w", ns, err)
		}
		log.Printf("secrets: made the key of %s", ns)
	} else if err != nil {
		return nil, err
	}
	own, err := publicKey(file)
	if err != nil {
		return nil, err
	}
	out := []string{own}
	data, err := os.ReadFile(filepath.Join(k.Dir, RecoveryFile))
	switch {
	case err == nil:
		r := strings.TrimSpace(string(data))
		if !recipient.MatchString(r) {
			return nil, fmt.Errorf("%s holds no age public key", RecoveryFile)
		}
		out = append(out, r)
	case !errors.Is(err, os.ErrNotExist):
		return nil, err
	}
	return out, nil
}

// publicKey reads the public key from age-keygen's comment in a key file.
func publicKey(file string) (string, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return "", err
	}
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		if pub, ok := strings.CutPrefix(sc.Text(), "# public key: "); ok && recipient.MatchString(pub) {
			return pub, nil
		}
	}
	return "", fmt.Errorf("%s names no public key", filepath.Base(file))
}

// Has tells whether a namespace has a key yet.
func (k *Keys) Has(ns string) bool {
	if !namespace.MatchString(ns) {
		return false
	}
	_, err := os.Stat(k.file(ns))
	return err == nil
}

// Decrypt reads the value of a namespace's secret file.
func (k *Keys) Decrypt(ctx context.Context, ns string, data []byte) (string, error) {
	if !k.Has(ns) {
		return "", fmt.Errorf("%s has no key here: its secrets were encrypted elsewhere", ns)
	}
	plain, err := k.Run(ctx, []string{"SOPS_AGE_KEY_FILE=" + k.file(ns)}, data, "sops", "decrypt",
		"--input-type", "yaml", "--output-type", "yaml", "/dev/stdin")
	if err != nil {
		return "", fmt.Errorf("decrypt: %w", err)
	}
	return value(plain)
}

// Handler answers GET /secrets/{namespace}/recipients with the public keys
// to encrypt a namespace's secrets for, as a JSON list, making its key if
// it has none. exists says whether a namespace exists, so no key is made
// for one that does not. Public keys are public: the dashboard asks.
func (k *Keys) Handler(exists func(ctx context.Context, ns string) (bool, error)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ns := r.PathValue("namespace")
		if !namespace.MatchString(ns) {
			http.Error(w, "invalid namespace", http.StatusBadRequest)
			return
		}
		ok, err := exists(r.Context(), ns)
		if err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		if !ok {
			http.Error(w, "no such namespace", http.StatusNotFound)
			return
		}
		out, err := k.Recipients(r.Context(), ns)
		if err != nil {
			log.Printf("secrets: recipients of %s: %v", ns, err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(out)
	})
}
