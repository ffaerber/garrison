package gitops

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"garrison/control-plane/internal/secrets"
)

// syncLogins writes the units' site logins, secrets/login-<name>.yaml in
// each one's config (cfgDirs: unit -> its checkout of main), to
// dir/<unit>/<name>.json, root only, where the gateway's browser reads them;
// the agents never do. It writes a file only when it changed, and removes
// the files whose secret is gone. dir holds nothing else.
func syncLogins(ctx context.Context, keys Decrypter, cfgDirs map[string]string, dir string) error {
	want := map[string]string{} // unit/name.json -> the login
	var errs []error
	for ns, cfg := range cfgDirs {
		files, _ := filepath.Glob(filepath.Join(cfg, secrets.Dir, secrets.LoginPrefix+"*.yaml"))
		for _, f := range files {
			name, ok := secrets.NameOf(secrets.Dir + "/" + filepath.Base(f))
			if !ok {
				continue
			}
			data, err := os.ReadFile(f)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			value, err := keys.Decrypt(ctx, ns, data)
			if err != nil {
				errs = append(errs, fmt.Errorf("the login %s of %s: %w", name, ns, err))
				continue
			}
			if _, err := secrets.ParseLogin(value); err != nil {
				errs = append(errs, fmt.Errorf("the secret %s of %s: %w", name, ns, err))
				continue
			}
			want[filepath.Join(ns, strings.TrimPrefix(name, secrets.LoginPrefix)+".json")] = value
		}
	}
	for rel, value := range want {
		path := filepath.Join(dir, rel)
		if have, err := os.ReadFile(path); err == nil && string(have) == value {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			errs = append(errs, err)
			continue
		}
		if err := writeKey(path, value); err != nil {
			errs = append(errs, err)
			continue
		}
		log.Printf("logins: wrote %s", strings.TrimSuffix(rel, ".json"))
	}
	have, _ := filepath.Glob(filepath.Join(dir, "*", "*.json"))
	for _, path := range have {
		rel, _ := filepath.Rel(dir, path)
		if _, ok := want[rel]; ok {
			continue
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
			continue
		}
		log.Printf("logins: removed %s: its secret is gone", strings.TrimSuffix(rel, ".json"))
	}
	return errors.Join(errs...)
}
