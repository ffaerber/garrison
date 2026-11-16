// Package secrets keeps a namespace's secrets: files secrets/<name>.yaml in
// its config repo, encrypted with SOPS for two age keys. One is the
// namespace's own, which only the deployer holds (Keys); the other is the
// installation's recovery key, whose private half is shown once at install
// and kept offline. Everyone signed in can read the repo: they see the
// names, never the values.
//
// A person sets a value in the dashboard's write-only form (Store): it is
// encrypted with the public keys alone, so the dashboard can write secrets
// but never read them, and committed to config's main branch. The deployer
// decrypts a secret only to hand it to the main environment of the stacks
// that name it, as a Swarm secret.
package secrets

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// Dir is where a namespace's secrets are, in its config repo.
const Dir = "secrets"

// SOPSConfig is the file in config that tells sops whom to encrypt secrets/
// for, so that a person with sops can also encrypt one by hand.
const SOPSConfig = ".sops.yaml"

// MaxValue is the largest value a secret holds: Swarm takes up to 500 KiB,
// but a secret is a key, a password or a certificate, not a file store.
const MaxValue = 64 << 10

// Name is what a secret may be called: as a service names it under
// `secrets:` in a stack file, and short enough for the Swarm secret's name
// (at most 64 characters), which adds the environment's hostname and a
// version.
var Name = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$`)

// Path is a secret's file in config.
func Path(name string) string { return Dir + "/" + name + ".yaml" }

// NameOf is the secret a path in config holds, if it holds one.
func NameOf(path string) (string, bool) {
	name, ok := strings.CutPrefix(path, Dir+"/")
	if !ok {
		return "", false
	}
	name, ok = strings.CutSuffix(name, ".yaml")
	return name, ok && Name.MatchString(name)
}

// Runner runs a command with extra environment and stdin, and returns its
// stdout (apply.Exec, or a fake in tests).
type Runner func(ctx context.Context, env []string, stdin []byte, name string, args ...string) ([]byte, error)

// recipient is an age public key.
var recipient = regexp.MustCompile(`^age1[0-9a-z]{58}$`)

// Encrypt makes the file of a secret holding value, for recipients (age
// public keys). The value goes to sops on stdin, never on a command line.
func Encrypt(ctx context.Context, run Runner, recipients []string, value string) ([]byte, error) {
	if len(recipients) == 0 {
		return nil, fmt.Errorf("no key to encrypt for")
	}
	for _, r := range recipients {
		if !recipient.MatchString(r) {
			return nil, fmt.Errorf("not an age public key: %q", r)
		}
	}
	plain, err := yaml.Marshal(map[string]string{"value": value})
	if err != nil {
		return nil, err
	}
	out, err := run(ctx, nil, plain, "sops", "encrypt", "--age", strings.Join(recipients, ","),
		"--input-type", "yaml", "--output-type", "yaml", "/dev/stdin")
	if err != nil {
		return nil, fmt.Errorf("encrypt: %w", err)
	}
	return out, nil
}

// SOPSFile is config's .sops.yaml for recipients.
func SOPSFile(domain string, recipients []string) string {
	return "# How secrets/ is encrypted: for this namespace's key, which only the\n" +
		"# deployer holds, and the installation's recovery key. Set and replace\n" +
		"# secrets at https://" + domain + "/secrets; with sops, `sops encrypt` reads\n" +
		"# this file. Nobody can read a value back.\n" +
		"creation_rules:\n" +
		"  - path_regex: ^secrets/[^/]+\\.yaml$\n" +
		"    age: " + strings.Join(recipients, ",") + "\n"
}

// value reads a decrypted secret file.
func value(plain []byte) (string, error) {
	var doc struct {
		Value *string `yaml:"value"`
	}
	if err := yaml.Unmarshal(plain, &doc); err != nil {
		return "", fmt.Errorf("decrypted, but not a secret file: %w", err)
	}
	if doc.Value == nil {
		return "", fmt.Errorf("decrypted, but it holds no value")
	}
	return *doc.Value, nil
}
