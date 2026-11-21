package secrets

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"garrison/control-plane/internal/models"
)

// ErrForbidden: the person may not manage the unit's secrets.
var ErrForbidden = errors.New("only the unit's admins and root admins manage its secrets")

// ErrInvalid: a name or value that cannot be a secret.
var ErrInvalid = errors.New("invalid secret")

// Gitea is what the store needs of Gitea, as the platform's own account.
type Gitea interface {
	IsAdmin(ctx context.Context, person string) (bool, error)
	// Role is "admin", "member" or "" (members.yaml on config's main).
	Role(ctx context.Context, ns, person string) (string, error)
	// Orgs are the organisations person sees.
	Orgs(ctx context.Context, person string) ([]string, error)
	// Files are the names of the files in a directory of a repo's main
	// branch; none if it does not exist.
	Files(ctx context.Context, org, repo, dir string) ([]string, error)
	// File is a file of a repo's main branch; "" if it does not exist.
	File(ctx context.Context, org, repo, path string) (string, error)
	// Commit writes files (nil: deletes) to a repo's main branch in one
	// commit, authored by author.
	Commit(ctx context.Context, org, repo, message, author string, files map[string][]byte) error
}

// Store writes a unit's secrets for the dashboard's form: it encrypts
// with the public keys alone, so it can write secrets but never read them.
type Store struct {
	Gitea Gitea
	// Recipients are the public keys of a unit (the deployer's
	// Keys.Handler, through Remote).
	Recipients func(ctx context.Context, ns string) ([]string, error)
	Run        Runner
	Domain     string
}

// PlatformUnit has no apps of its config. Its secrets are only the
// LLM providers' API keys, llm-<provider> (models.KeySecret), which root
// admins set on the dashboard's Models page and the deployer writes to the
// agent service's key folder.
const PlatformUnit = "garrison"

// ErrPlatformName: a secret of garrison that is not a provider's key.
var ErrPlatformName = fmt.Errorf("%w: garrison keeps only the LLM providers' keys, llm-<provider>, set on the Models page", ErrInvalid)

// ConfigRepo is the repo a unit's secrets are in.
const ConfigRepo = "config"

// May tells whether person may manage the secrets of ns: its admins and
// root admins; garrison's, its providers' keys, root admins only.
func (s *Store) May(ctx context.Context, ns, person string) (bool, error) {
	if !unit.MatchString(ns) {
		return false, nil
	}
	admin, err := s.Gitea.IsAdmin(ctx, person)
	if err != nil || admin || ns == PlatformUnit {
		return admin, err
	}
	role, err := s.Gitea.Role(ctx, ns, person)
	return role == "admin", err
}

// Units are those whose secrets person may manage on the Secrets page:
// garrison's are on the Models page.
func (s *Store) Units(ctx context.Context, person string) ([]string, error) {
	orgs, err := s.Gitea.Orgs(ctx, person)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, ns := range orgs {
		if ns == PlatformUnit {
			continue
		}
		ok, err := s.May(ctx, ns, person)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, ns)
		}
	}
	slices.Sort(out)
	return out, nil
}

// Names are the secrets a unit has.
func (s *Store) Names(ctx context.Context, ns string) ([]string, error) {
	files, err := s.Gitea.Files(ctx, ns, ConfigRepo, Dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, f := range files {
		if name, ok := NameOf(Dir + "/" + f); ok {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out, nil
}

// CheckValue tells why value cannot be a secret, if it cannot.
func CheckValue(value string) error {
	switch {
	case value == "":
		return fmt.Errorf("%w: the value is empty", ErrInvalid)
	case len(value) > MaxValue:
		return fmt.Errorf("%w: the value is longer than %d KiB", ErrInvalid, MaxValue>>10)
	case !utf8.ValidString(value):
		return fmt.Errorf("%w: the value is not text", ErrInvalid)
	}
	return nil
}

// Set encrypts value and commits it as the secret name of ns, for person.
func (s *Store) Set(ctx context.Context, ns, name, value, person string) error {
	if !Name.MatchString(name) {
		return fmt.Errorf("%w: a name is lowercase letters, digits and dashes, up to 32", ErrInvalid)
	}
	if _, ok := models.ProviderOf(name); ns == PlatformUnit && !ok {
		return ErrPlatformName
	}
	if err := CheckValue(value); err != nil {
		return err
	}
	if ok, err := s.May(ctx, ns, person); err != nil {
		return err
	} else if !ok {
		return ErrForbidden
	}
	recipients, err := s.Recipients(ctx, ns)
	if err != nil {
		return fmt.Errorf("the keys of %s: %w", ns, err)
	}
	enc, err := Encrypt(ctx, s.Run, recipients, value)
	if err != nil {
		return err
	}
	files := map[string][]byte{Path(name): enc}
	// .sops.yaml says whom secrets/ is for; kept with the keys.
	want := SOPSFile(s.Domain, recipients)
	if have, err := s.Gitea.File(ctx, ns, ConfigRepo, SOPSConfig); err != nil {
		return err
	} else if have != want {
		files[SOPSConfig] = []byte(want)
	}
	return s.Gitea.Commit(ctx, ns, ConfigRepo, fmt.Sprintf("Set the secret %s, for %s", name, person), person, files)
}

// Delete removes the secret name of ns, for person.
func (s *Store) Delete(ctx context.Context, ns, name, person string) error {
	if !Name.MatchString(name) {
		return fmt.Errorf("%w: no secret is called %q", ErrInvalid, name)
	}
	if _, ok := models.ProviderOf(name); ns == PlatformUnit && !ok {
		return ErrPlatformName
	}
	if ok, err := s.May(ctx, ns, person); err != nil {
		return err
	} else if !ok {
		return ErrForbidden
	}
	names, err := s.Names(ctx, ns)
	if err != nil {
		return err
	}
	if !slices.Contains(names, name) {
		return fmt.Errorf("%w: %s has no secret %s", ErrInvalid, ns, name)
	}
	return s.Gitea.Commit(ctx, ns, ConfigRepo, fmt.Sprintf("Delete the secret %s, for %s", name, person), person, map[string][]byte{Path(name): nil})
}

// Remote asks the deployer at base (e.g. http://deployer:8090)
// for a unit's public keys.
func Remote(base string) func(ctx context.Context, ns string) ([]string, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	return func(ctx context.Context, ns string) ([]string, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+"/secrets/"+url.PathEscape(ns)+"/recipients", nil)
		if err != nil {
			return nil, err
		}
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("the deployer: %s %s", resp.Status, strings.TrimSpace(string(body)))
		}
		var out []string
		if err := json.Unmarshal(body, &out); err != nil {
			return nil, err
		}
		return out, nil
	}
}
