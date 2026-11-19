package render

import (
	"flag"
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"garrison/control-plane/internal/stack"
)

var update = flag.Bool("update", false, "rewrite golden files")

func TestSwarmGolden(t *testing.T) {
	data, err := os.ReadFile("../stack/testdata/jellyfin.yaml")
	if err != nil {
		t.Fatal(err)
	}
	s, err := stack.Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	for _, env := range []string{"main", "dev"} {
		t.Run(env, func(t *testing.T) {
			got, err := Swarm(s, Options{Environment: env, Domain: "example.com", DataRoot: "/tank"})
			if err != nil {
				t.Fatal(err)
			}
			golden := "testdata/jellyfin." + env + ".compose.yaml"
			if *update {
				if err := os.WriteFile(golden, got, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != string(want) {
				t.Errorf("rendered %s differs from %s:\n%s", env, golden, got)
			}
		})
	}
}

func TestSwarmErrors(t *testing.T) {
	s, err := stack.Parse([]byte("stack: a\nnamespace: n\nentrypoint: w\nservices:\n  w: { image: i:1, port: 1 }\nenvironments:\n  main: {}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Swarm(s, Options{Environment: "dev", Domain: "example.com"}); err == nil {
		t.Error("unknown environment accepted")
	}
	if _, err := Swarm(s, Options{Environment: "main"}); err == nil {
		t.Error("missing domain accepted")
	}
}

// The placeholders are filled in with or without spaces inside the braces:
// the stack's check counts both as {{commit}}.
func TestImagePlaceholders(t *testing.T) {
	for image, want := range map[string]string{
		"{{registry}}/homepage/todo:{{commit}}":     "git.example.com/homepage/todo:aaaaaaaaaaaa",
		"{{ registry }}/homepage/todo:{{ commit }}": "git.example.com/homepage/todo:aaaaaaaaaaaa",
		"postgres:17-alpine":                        "postgres:17-alpine",
	} {
		if got := Image(image, "example.com", "aaaaaaaaaaaa1111"); got != want {
			t.Errorf("Image(%q) = %q, want %q", image, got, want)
		}
	}
}

// A private environment's routed services get the ingress key, and Traefik
// puts it into every request it passes on after the sign-in; a public
// environment and an internal service get neither.
func TestTheIngressKeyOfPrivateEnvironments(t *testing.T) {
	s, err := stack.Parse([]byte(`stack: todo
namespace: homepage
entrypoint: web
services:
  web: { image: "todo:1", port: 8080, env: { MODE: "x" } }
  api: { image: "todo:1", port: 8081, public: api }
  db: { image: "postgres:17-alpine" }
environments:
  main: { private: true }
  dev: {}
`))
	if err != nil {
		t.Fatal(err)
	}
	render := func(env string) map[string]composeService {
		t.Helper()
		out, err := Swarm(s, Options{Environment: env, Domain: "example.com", IngressKey: "abc123"})
		if err != nil {
			t.Fatal(err)
		}
		var file composeFile
		if err := yaml.Unmarshal(out, &file); err != nil {
			t.Fatal(err)
		}
		return file.Services
	}
	private := render("main")
	for svc, host := range map[string]string{"web": "todo", "api": "todo-api"} {
		c := private[svc]
		l := c.Deploy.Labels
		if c.Environment[IngressKeyEnv] != "abc123" ||
			l["traefik.http.middlewares."+host+"-ingress.headers.customrequestheaders.X-Garrison-Ingress"] != "abc123" ||
			l["traefik.http.routers."+host+".middlewares"] != "sso@file,"+host+"-ingress@swarm" {
			t.Errorf("%s: env %v, labels %v", svc, c.Environment, l)
		}
	}
	if private["web"].Environment["MODE"] != "x" {
		t.Errorf("web lost its own environment: %v", private["web"].Environment)
	}
	if _, has := private["db"].Environment[IngressKeyEnv]; has {
		t.Error("an internal service got the key")
	}
	public := render("dev")
	for name, c := range public {
		if _, has := c.Environment[IngressKeyEnv]; has {
			t.Errorf("%s of a public environment got the key", name)
		}
		for k := range c.Deploy.Labels {
			if strings.Contains(k, "ingress") || strings.Contains(c.Deploy.Labels[k], "ingress") {
				t.Errorf("%s of a public environment: label %s", name, k)
			}
		}
	}
}

func TestMembersInEnv(t *testing.T) {
	s, err := stack.ParseIn([]byte("entrypoint: web\nservices:\n  web: { image: x/y:1, port: 80, env: { WRITERS: \"{{members}}\", OTHER: plain } }\nenvironments:\n  main: {}\n"), "media", "files")
	if err != nil {
		t.Fatal(err)
	}
	out, err := Swarm(s, Options{Environment: "main", Domain: "home.test", Members: []string{"alice", "bob"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "WRITERS: alice,bob") || !strings.Contains(string(out), "OTHER: plain") {
		t.Errorf("%s", out)
	}
}
