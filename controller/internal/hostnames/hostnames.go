// Package hostnames tracks which stack holds which hostname.
//
// A stack claims exactly the hostnames it declares: one per environment plus
// one per public service. Nothing is claimed by prefix. Claims are first
// come, first served across all namespaces, and root can reserve names.
package hostnames

import (
	"errors"
	"fmt"

	"homecloud/controller/internal/stack"
)

type Claim struct {
	Hostname    string
	Namespace   string
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
				out = append(out, Claim{h, s.Namespace, s.Name, env, svc})
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
				c.Hostname, h.Namespace, h.Stack, h.Environment))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return err
	}
	r.Release(s.Namespace, s.Name)
	for _, c := range claims {
		r.held[c.Hostname] = c
	}
	return nil
}

// Release drops every hostname held by a stack.
func (r *Registry) Release(namespace, name string) {
	for h, c := range r.held {
		if c.Namespace == namespace && c.Stack == name {
			delete(r.held, h)
		}
	}
}

func sameStack(a, b Claim) bool { return a.Namespace == b.Namespace && a.Stack == b.Stack }
