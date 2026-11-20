// Package hostnames tracks which stack holds which hostname.
//
// A stack claims exactly the hostnames it declares: one per environment plus
// one per public service. Nothing is claimed by prefix. Claims are first
// come, first served across all units, and root can reserve names.
package hostnames

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"garrison/control-plane/internal/stack"
)

type Claim struct {
	Hostname    string
	Unit        string
	Stack       string
	Environment string
	Service     string
}

// Of lists every hostname a stack claims, ordered by environment and service.
func Of(s *stack.Stack) []Claim {
	var out []Claim
	for _, env := range s.EnvironmentNames() {
		for _, svc := range s.ServiceNames() {
			if h := s.ServiceHostname(env, svc); h != "" {
				out = append(out, Claim{h, s.Unit, s.Name, env, svc})
			}
		}
	}
	return out
}

type Registry struct {
	reserved map[string]bool
	held     map[string]Claim
}

func NewRegistry(reserved []string) *Registry {
	r := &Registry{reserved: map[string]bool{}, held: map[string]Claim{}}
	for _, h := range reserved {
		r.reserved[h] = true
	}
	return r
}

// Claim records the hostnames of s, replacing whatever s held before. It
// changes nothing and returns an error if any hostname is reserved or held by
// another stack.
func (r *Registry) Claim(s *stack.Stack) error {
	claims := Of(s)
	var errs []error
	for _, c := range claims {
		if r.reserved[c.Hostname] {
			errs = append(errs, fmt.Errorf("hostname %q is reserved by root", c.Hostname))
			continue
		}
		if h, ok := r.held[c.Hostname]; ok && !sameStack(h, c) {
			errs = append(errs, fmt.Errorf("hostname %q is already held by stack %s/%s (environment %s)",
				c.Hostname, h.Unit, h.Stack, h.Environment))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return err
	}
	r.Release(s.Unit, s.Name)
	for _, c := range claims {
		r.held[c.Hostname] = c
	}
	return nil
}

// Release drops every hostname held by a stack.
func (r *Registry) Release(unit, name string) {
	for h, c := range r.held {
		if c.Unit == unit && c.Stack == name {
			delete(r.held, h)
		}
	}
}

func sameStack(a, b Claim) bool { return a.Unit == b.Unit && a.Stack == b.Stack }

// Load reads the held claims saved by Save. A missing file is an empty registry.
func (r *Registry) Load(file string) error {
	data, err := os.ReadFile(file)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var claims []Claim
	if err := json.Unmarshal(data, &claims); err != nil {
		return fmt.Errorf("%s: %w", file, err)
	}
	for _, c := range claims {
		r.held[c.Hostname] = c
	}
	return nil
}

// Save writes the held claims, sorted by hostname.
func (r *Registry) Save(file string) error {
	claims := make([]Claim, 0, len(r.held))
	for _, c := range r.held {
		claims = append(claims, c)
	}
	slices.SortFunc(claims, func(a, b Claim) int { return strings.Compare(a.Hostname, b.Hostname) })
	data, err := json.MarshalIndent(claims, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		return err
	}
	tmp := file + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, file)
}
