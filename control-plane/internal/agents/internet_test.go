package agents

import (
	"context"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestInternetInAgentYAML(t *testing.T) {
	for raw, want := range map[string]Internet{
		"":                                      {},
		"internet: packages":                    {Packages: true},
		"internet: all":                         {All: true},
		"internet: [packages, API.Example.com]": {Packages: true, Domains: []string{"api.example.com"}},
		"internet: [example.org, example.org]":  {Domains: []string{"example.org"}},
	} {
		var c struct {
			Internet Internet `yaml:"internet"`
		}
		if err := yaml.Unmarshal([]byte(raw), &c); err != nil {
			t.Errorf("%q: %v", raw, err)
			continue
		}
		if c.Internet.All != want.All || c.Internet.Packages != want.Packages || !slices.Equal(c.Internet.Domains, want.Domains) {
			t.Errorf("%q: %+v", raw, c.Internet)
		}
		// It writes back as read.
		out, _ := yaml.Marshal(c)
		var back struct {
			Internet Internet `yaml:"internet"`
		}
		yaml.Unmarshal(out, &back)
		if back.Internet.String() != c.Internet.String() {
			t.Errorf("%q: written as %s", raw, out)
		}
	}
	for _, raw := range []string{
		"internet: yes", "internet: [all]", "internet: [packages, http://example.org]",
		"internet: [10.0.0.1]", "internet: [localhost]", "internet: {all: true}", "internet: [*.example.org]",
	} {
		var c struct {
			Internet Internet `yaml:"internet"`
		}
		if err := yaml.Unmarshal([]byte(raw), &c); err == nil {
			t.Errorf("%q: accepted as %+v", raw, c.Internet)
		}
	}
}

func TestInternetAllows(t *testing.T) {
	packages := Internet{Packages: true}
	mine := Internet{Packages: true, Domains: []string{"example.org"}}
	for _, c := range []struct {
		n    Internet
		host string
		want bool
	}{
		{packages, "pypi.org", true},
		{packages, "files.pythonhosted.org", true},
		{packages, "dl-cdn.alpinelinux.org", true},
		{packages, "PyPI.org.", true},
		{packages, "github.com", false},
		{packages, "evilpypi.org", false},
		{packages, "pypi.org.evil.com", false},
		{mine, "example.org", true},
		{mine, "api.example.org", true},
		{mine, "notexample.org", false},
		{Internet{}, "pypi.org", false},
		{Internet{All: true}, "anything.example", true},
	} {
		if got := c.n.Allows(c.host); got != c.want {
			t.Errorf("%s allows %s: %v", c.n, c.host, got)
		}
	}
	if n := (Internet{All: true}).Narrowed(); n.All || !n.Packages || n.Wide() {
		t.Errorf("all narrowed: %+v", n)
	}
	if n := (Internet{Packages: true, Domains: []string{"api.example.com"}}).Narrowed(); !n.Packages || n.Wide() {
		t.Errorf("packages and a domain narrowed to %+v", n)
	}
	// Domains without the registries: narrowing gives none it never had.
	if n := (Internet{Domains: []string{"api.example.com"}}).Narrowed(); !n.None() {
		t.Errorf("a domain alone narrowed to %+v", n)
	}
	if n := (Internet{}).Narrowed(); !n.None() {
		t.Errorf("none narrowed: %+v", n)
	}
}

func TestAnAgentSaysWhatItReaches(t *testing.T) {
	files := mapFiles{
		"agents/web/agent.yaml":     "internet: [packages, example.org]\n",
		"agents/web/prompt.md":      "You look things up.",
		"agents/offline/agent.yaml": "description: d\n",
		"agents/offline/prompt.md":  "You stay here.",
		"agents/bad/agent.yaml":     "internet: everything\n",
		"agents/bad/prompt.md":      "p",
	}
	list, problems, err := Load(context.Background(), files, "homepage", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(problems) != 1 || !strings.Contains(problems[0], `agents/bad: agent.yaml: line 1: internet: "everything"`) {
		t.Errorf("problems %q", problems)
	}
	for _, a := range list {
		s := System(a, "example.com")
		switch a.Name {
		case "web":
			if !strings.Contains(s, "Your sandbox reaches the package registries") || !strings.Contains(s, "example.org (each with its subdomains)") || strings.Contains(s, "You have no internet") {
				t.Errorf("web: %s", s)
			}
		case "offline":
			if !strings.Contains(s, "You have no internet") {
				t.Errorf("offline: %s", s)
			}
		}
	}
}
