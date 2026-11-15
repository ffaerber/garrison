package agents

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// Internet is what of the internet an agent's sandbox reaches, through the
// agent service's proxy. The zero value is nothing.
//
// In agent.yaml:
//
//	internet: packages                      # the package registries
//	internet: all                           # every public address
//	internet: [packages, api.example.com]   # registries and these domains
//
// A domain counts with its subdomains. Agents that work on private repos
// reach the registries at most: the approval check refuses more, and the
// proxy holds them to it should a repo turn private later.
type Internet struct {
	All      bool
	Packages bool
	Domains  []string
}

// Values of internet in agent.yaml.
const (
	InternetAll      = "all"
	InternetPackages = "packages"
)

// PackageDomains are the registries "packages" opens: Alpine, Debian and
// Ubuntu, PyPI, npm, Go modules and Rust crates. Only download hosts: no
// GitHub, whose gists and pushes would let a tricked agent send code out.
var PackageDomains = []string{
	"dl-cdn.alpinelinux.org",
	"deb.debian.org", "security.debian.org",
	"archive.ubuntu.com", "security.ubuntu.com", "ports.ubuntu.com",
	"pypi.org", "files.pythonhosted.org",
	"registry.npmjs.org", "registry.yarnpkg.com",
	"proxy.golang.org", "sum.golang.org",
	"index.crates.io", "static.crates.io", "static.rust-lang.org",
}

var domainName = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,63}$`)

// UnmarshalYAML reads a word or a list.
func (n *Internet) UnmarshalYAML(v *yaml.Node) error {
	var items []string
	switch v.Kind {
	case yaml.ScalarNode:
		if v.Value == InternetAll {
			*n = Internet{All: true}
			return nil
		}
		items = []string{v.Value}
	case yaml.SequenceNode:
		if err := v.Decode(&items); err != nil {
			return err
		}
	default:
		return fmt.Errorf("line %d: internet is packages, all, or a list of packages and domains", v.Line)
	}
	out := Internet{}
	for _, it := range items {
		it = strings.ToLower(strings.TrimSpace(it))
		switch {
		case it == InternetPackages:
			out.Packages = true
		case it == InternetAll:
			return fmt.Errorf("line %d: internet: all stands alone, not in a list", v.Line)
		case domainName.MatchString(it) && len(it) <= 253:
			if !slices.Contains(out.Domains, it) {
				out.Domains = append(out.Domains, it)
			}
		default:
			return fmt.Errorf("line %d: internet: %q is neither packages nor a domain such as api.example.com", v.Line, it)
		}
	}
	*n = out
	return nil
}

// MarshalYAML writes it as read.
func (n Internet) MarshalYAML() (any, error) {
	switch {
	case n.All:
		return InternetAll, nil
	case n.Packages && len(n.Domains) == 0:
		return InternetPackages, nil
	case n.None():
		return nil, nil
	}
	var items []string
	if n.Packages {
		items = append(items, InternetPackages)
	}
	return append(items, n.Domains...), nil
}

// None: no internet at all.
func (n Internet) None() bool { return !n.All && !n.Packages && len(n.Domains) == 0 }

// Wide: more than the registries.
func (n Internet) Wide() bool { return n.All || len(n.Domains) > 0 }

// Narrowed is what an agent on private repos keeps: the registries at
// most, and only if it had them, which all includes.
func (n Internet) Narrowed() Internet {
	return Internet{Packages: n.Packages || n.All}
}

// Allows tells whether host (a name, lower case, without port) may be
// reached. Addresses are for All only; the proxy also refuses private ones.
func (n Internet) Allows(host string) bool {
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if n.All {
		return true
	}
	under := func(list []string) bool {
		return slices.ContainsFunc(list, func(d string) bool { return host == d || strings.HasSuffix(host, "."+d) })
	}
	return (n.Packages && under(PackageDomains)) || under(n.Domains)
}

// String says it in words, for prompts and logs.
func (n Internet) String() string {
	switch {
	case n.All:
		return "the whole internet"
	case n.None():
		return "no internet"
	}
	var parts []string
	if n.Packages {
		parts = append(parts, "the package registries (Alpine, Debian, Ubuntu, PyPI, npm, Go, crates)")
	}
	if len(n.Domains) > 0 {
		parts = append(parts, strings.Join(n.Domains, ", ")+" (each with its subdomains)")
	}
	return strings.Join(parts, " and ")
}
