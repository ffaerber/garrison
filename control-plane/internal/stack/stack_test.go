package stack

import (
	"os"
	"strings"
	"testing"
)

func TestParseSpecExample(t *testing.T) {
	data, err := os.ReadFile("testdata/jellyfin.yaml")
	if err != nil {
		t.Fatal(err)
	}
	s, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ env, service, want string }{
		{"main", "app", "jellyfin"},
		{"main", "admin", "jellyfin-admin"},
		{"main", "redis", ""},
		{"dev", "app", "dev-jellyfin"},
		{"dev", "admin", "dev-jellyfin-admin"},
	} {
		if got := s.ServiceHostname(c.env, c.service); got != c.want {
			t.Errorf("ServiceHostname(%s, %s) = %q, want %q", c.env, c.service, got, c.want)
		}
	}
	if got := s.Services["app"].Volumes["config"].MountPath("config"); got != "/config" {
		t.Errorf("default mount path = %q, want /config", got)
	}
}

const minimal = `
stack: site
namespace: homepage
entrypoint: web
services:
  web: { image: nginx:1, port: 80 }
environments:
  main: {}
`

func TestParseDefaultsHostname(t *testing.T) {
	s, err := Parse([]byte(minimal + "  dev: {}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := s.Environments["main"].Hostname; got != "site" {
		t.Errorf("main hostname = %q, want site", got)
	}
	if got := s.Environments["dev"].Hostname; got != "dev-site" {
		t.Errorf("dev hostname = %q, want dev-site", got)
	}
}

func TestParseRejects(t *testing.T) {
	cases := map[string]struct{ yaml, want string }{
		"policy block": {
			minimal + "policy: { autonomy: autonomous }\n",
			"policy",
		},
		"unknown entrypoint": {
			strings.Replace(minimal, "entrypoint: web", "entrypoint: api", 1),
			`entrypoint "api" is not a service`,
		},
		"entrypoint without port": {
			strings.Replace(minimal, ", port: 80", "", 1),
			"needs a port",
		},
		"hostname not following the rule": {
			minimal + "  dev: { hostname: site-dev }\n",
			`must have hostname "dev-site"`,
		},
		"uppercase name": {
			strings.Replace(minimal, "stack: site", "stack: Site", 1),
			"schema",
		},
		"public without port": {
			strings.Replace(minimal, "environments:", "  admin: { image: a:1, public: admin }\nenvironments:", 1),
			"is public and needs a port",
		},
		"duplicate public name": {
			strings.Replace(minimal, "environments:",
				"  a: { image: a:1, port: 1, public: x }\n  b: { image: b:1, port: 2, public: x }\nenvironments:", 1),
			"both use public name",
		},
		"two volumes at one path": {
			strings.Replace(minimal, "environments:",
				"  db:\n    image: d:1\n    volumes:\n      a: { dataset: a, size: 1Gi, path: /data }\n      b: { dataset: b, size: 1Gi, path: /data }\nenvironments:", 1),
			"at the same path",
		},
		"volume that is both dataset and share": {
			strings.Replace(minimal, "environments:",
				"  db:\n    image: d:1\n    volumes:\n      a: { dataset: a, size: 1Gi, share: lib }\nenvironments:", 1),
			"schema",
		},
		"hostname too long": {
			strings.Replace(minimal, "stack: site", "stack: "+strings.Repeat("a", 60), 1) + "  staging: {}\n",
			"longer than 63",
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(c.yaml))
			if err == nil {
				t.Fatal("Parse accepted an invalid stack")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error %q does not mention %q", err, c.want)
			}
		})
	}
}
