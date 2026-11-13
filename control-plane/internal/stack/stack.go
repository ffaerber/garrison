// Package stack parses and validates the platform's stack format.
package stack

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"gopkg.in/yaml.v3"

	"homecloud/schemas"
)

// MainEnvironment is the environment whose hostname is the stack name.
const MainEnvironment = "main"

// PreviewEnvironment, if a stack has it, is no branch: each open pull
// request of the repo is deployed like it, as the environment pr-<number>
// at pr-<number>-<stack>.<domain>, and taken down when the pull request
// closes.
const PreviewEnvironment = "preview"

var previewName = regexp.MustCompile(`^pr-[0-9]+$`)

// PreviewName is the environment of pull request n.
func PreviewName(n int) string { return fmt.Sprintf("pr-%d", n) }

// Branches are the environments deployed from a branch of their name.
func (s *Stack) Branches() []string {
	return slices.DeleteFunc(s.EnvironmentNames(), func(e string) bool { return e == PreviewEnvironment })
}

// WithPreviews makes the environments of the previews names (pr-<n>) like
// the preview environment, and drops it, as it is deployed only through
// them. It does nothing to a stack without previews.
func (s *Stack) WithPreviews(names []string) {
	p, ok := s.Environments[PreviewEnvironment]
	if !ok {
		return
	}
	delete(s.Environments, PreviewEnvironment)
	for _, n := range names {
		e := p
		e.Hostname = s.Hostname(n)
		s.Environments[n] = e
	}
}

// schemaURL only names the embedded schema in error messages.
const schemaURL = "homecloud:///schemas/stack.schema.json"

// maxLabel is the longest DNS label; every hostname is one label.
const maxLabel = 63

type Stack struct {
	Name         string                 `yaml:"stack"`
	Namespace    string                 `yaml:"namespace"`
	Source       string                 `yaml:"source,omitempty"` // the repo whose branches are its environments
	Entrypoint   string                 `yaml:"entrypoint"`
	Services     map[string]Service     `yaml:"services"`
	Environments map[string]Environment `yaml:"environments"`
}

type Service struct {
	Image     string            `yaml:"image"`
	Port      int               `yaml:"port"`
	Public    string            `yaml:"public"`
	Devices   []string          `yaml:"devices"`
	Resources Resources         `yaml:"resources"`
	Volumes   map[string]Volume `yaml:"volumes"`
	Secrets   []string          `yaml:"secrets"`
	Env       map[string]string `yaml:"env"`
}

type Resources struct {
	Memory string `yaml:"memory"`
}

// Volume is either a dataset (Dataset and Size set) or a share (Share set).
type Volume struct {
	Dataset  string `yaml:"dataset"`
	Size     string `yaml:"size"`
	Share    string `yaml:"share"`
	Readonly bool   `yaml:"readonly"`
	Path     string `yaml:"path"`
}

// MountPath is where the volume appears in the container.
func (v Volume) MountPath(name string) string {
	if v.Path != "" {
		return v.Path
	}
	return "/" + name
}

type Environment struct {
	Hostname string `yaml:"hostname"`
	Private  bool   `yaml:"private"`
}

var compiled = sync.OnceValue(func() *jsonschema.Schema {
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(schemas.Stack))
	if err != nil {
		panic(err)
	}
	c := jsonschema.NewCompiler()
	if err := c.AddResource(schemaURL, doc); err != nil {
		panic(err)
	}
	return c.MustCompile(schemaURL)
})

// Dir is where a namespace's stacks are in its config repo: one file each,
// stacks/<name>.yaml.
const Dir = "stacks"

// Parse validates a stack file that stands alone, e.g. one the host's CLI
// deploys: it names its stack and namespace.
func Parse(data []byte) (*Stack, error) {
	s, err := parse(data)
	if err != nil {
		return nil, err
	}
	if s.Name == "" || s.Namespace == "" {
		return nil, errors.New("a stack file on its own names its stack and namespace (stack:, namespace:)")
	}
	return s, nil
}

var fileName = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,38}[a-z0-9])?$`)

// ParseIn validates the stack stacks/<name>.yaml of a namespace's config
// repo, whose place names its stack and namespace. Without a source repo,
// it has only main, and no image of its own build.
func ParseIn(data []byte, namespace, name string) (*Stack, error) {
	if !fileName.MatchString(name) {
		return nil, fmt.Errorf("%s/%s.yaml: the name must be lowercase letters, digits and dashes, at most 40, as it is a hostname", Dir, name)
	}
	var raw map[string]any
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parse yaml: %w", err)
	}
	if raw == nil {
		raw = map[string]any{}
	}
	for key, want := range map[string]string{"stack": name, "namespace": namespace} {
		if v, ok := raw[key]; ok && v != want {
			return nil, fmt.Errorf("%s: %v, but the file is %s/%s.yaml of %s; leave it out", key, v, Dir, name, namespace)
		}
		raw[key] = want
	}
	full, err := yaml.Marshal(raw)
	if err != nil {
		return nil, err
	}
	s, err := parse(full)
	if err != nil {
		return nil, err
	}
	if s.Source == "" {
		var errs []error
		for _, env := range s.EnvironmentNames() {
			if env != MainEnvironment {
				errs = append(errs, fmt.Errorf("environment %s needs a source repo, whose branches and pull requests it follows; without one there is only main", env))
			}
		}
		for _, svc := range s.ServiceNames() {
			if img := s.Services[svc].Image; regexp.MustCompile(`\{\{\s*commit\s*\}\}`).MatchString(img) {
				errs = append(errs, fmt.Errorf("service %s: {{commit}} in its image needs a source repo, whose commits it names", svc))
			}
		}
		if err := errors.Join(errs...); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// parse validates a stack file against the JSON Schema and the rules the
// schema cannot express, and fills in default hostnames.
func parse(data []byte) (*Stack, error) {
	var raw any
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parse yaml: %w", err)
	}
	j, err := json.Marshal(raw)
	if err != nil {
		return nil, fmt.Errorf("parse yaml: %w", err)
	}
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(j))
	if err != nil {
		return nil, fmt.Errorf("parse yaml: %w", err)
	}
	if err := compiled().Validate(inst); err != nil {
		return nil, fmt.Errorf("schema: %w", err)
	}

	var s Stack
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&s); err != nil {
		return nil, fmt.Errorf("parse yaml: %w", err)
	}
	if err := s.check(); err != nil {
		return nil, err
	}
	for name, env := range s.Environments {
		env.Hostname = s.Hostname(name)
		s.Environments[name] = env
	}
	return &s, nil
}

func (s *Stack) check() error {
	var errs []error
	fail := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	if ep, ok := s.Services[s.Entrypoint]; !ok {
		fail("entrypoint %q is not a service", s.Entrypoint)
	} else {
		if ep.Port == 0 {
			fail("entrypoint service %q needs a port", s.Entrypoint)
		}
		if ep.Public != "" {
			fail("entrypoint service %q already gets the hostname and cannot set public", s.Entrypoint)
		}
	}

	publics := map[string]string{}
	for _, name := range s.ServiceNames() {
		svc := s.Services[name]
		if svc.Public != "" {
			if svc.Port == 0 {
				fail("service %q is public and needs a port", name)
			}
			if other, dup := publics[svc.Public]; dup {
				fail("services %q and %q both use public name %q", other, name, svc.Public)
			}
			publics[svc.Public] = name
		}
		paths := map[string]string{}
		for _, vol := range sortedKeys(svc.Volumes) {
			p := svc.Volumes[vol].MountPath(vol)
			if other, dup := paths[p]; dup {
				fail("service %q mounts volumes %q and %q at the same path %s", name, other, vol, p)
			}
			paths[p] = vol
		}
	}

	seen := map[string]string{}
	for _, env := range s.EnvironmentNames() {
		if previewName.MatchString(env) {
			fail("environment %q: pr-<number> are the previews of pull requests; name the branch otherwise", env)
		}
		want := s.Hostname(env)
		if got := s.Environments[env].Hostname; got != "" && got != want {
			fail("environment %q must have hostname %q, not %q", env, want, got)
		}
		for _, h := range s.hostnames(env) {
			if len(h) > maxLabel {
				fail("hostname %q is longer than %d characters", h, maxLabel)
			}
			if other, dup := seen[h]; dup {
				fail("hostname %q is used by both environment %q and environment %q", h, other, env)
			}
			seen[h] = env
		}
	}
	return errors.Join(errs...)
}

// Hostname is the hostname of an environment's entry point: the stack name on
// main, <environment>-<stack> elsewhere.
func (s *Stack) Hostname(env string) string {
	if env == MainEnvironment {
		return s.Name
	}
	return env + "-" + s.Name
}

// ServiceHostname is the hostname a service is reachable on in an
// environment, or "" if the service is internal.
func (s *Stack) ServiceHostname(env, service string) string {
	switch svc := s.Services[service]; {
	case service == s.Entrypoint:
		return s.Hostname(env)
	case svc.Public != "":
		return s.Hostname(env) + "-" + svc.Public
	}
	return ""
}

func (s *Stack) hostnames(env string) []string {
	var out []string
	for _, name := range s.ServiceNames() {
		if h := s.ServiceHostname(env, name); h != "" {
			out = append(out, h)
		}
	}
	return out
}

func (s *Stack) ServiceNames() []string     { return sortedKeys(s.Services) }
func (s *Stack) EnvironmentNames() []string { return sortedKeys(s.Environments) }

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
