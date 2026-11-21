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

func TestGolden(t *testing.T) {
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
			got, err := Kubernetes(s, Options{Environment: env, Domain: "example.com", DataRoot: "/tank"})
			if err != nil {
				t.Fatal(err)
			}
			golden := "testdata/jellyfin." + env + ".kubernetes.yaml"
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
	s, err := stack.Parse([]byte("stack: a\nunit: n\nentrypoint: w\nservices:\n  w: { image: i:1, port: 1 }\nenvironments:\n  main: {}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Kubernetes(s, Options{Environment: "dev", Domain: "example.com"}); err == nil {
		t.Error("unknown environment accepted")
	}
	if _, err := Kubernetes(s, Options{Environment: "main"}); err == nil {
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
unit: homepage
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
	// objects: the rendered documents, by kind and name.
	render := func(env string) map[string]map[string]any {
		t.Helper()
		out, err := Kubernetes(s, Options{Environment: env, Domain: "example.com", IngressKey: "abc123"})
		if err != nil {
			t.Fatal(err)
		}
		objs := map[string]map[string]any{}
		dec := yaml.NewDecoder(strings.NewReader(string(out)))
		for {
			var o map[string]any
			if dec.Decode(&o) != nil {
				break
			}
			objs[o["kind"].(string)+"/"+o["metadata"].(map[string]any)["name"].(string)] = o
		}
		return objs
	}
	envOf := func(objs map[string]map[string]any, svc string) map[string]string {
		d := objs["Deployment/"+svc]
		c := d["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)
		out := map[string]string{}
		list, _ := c["env"].([]any)
		for _, e := range list {
			m := e.(map[string]any)
			out[m["name"].(string)] = m["value"].(string)
		}
		return out
	}
	private := render("main")
	for svc, host := range map[string]string{"web": "todo", "api": "todo-api"} {
		ing := private["Ingress/"+svc]
		ann := ing["metadata"].(map[string]any)["annotations"].(map[string]any)
		mw := private["Middleware/"+svc+"-ingress"]
		if envOf(private, svc)[IngressKeyEnv] != "abc123" || mw == nil ||
			mw["spec"].(map[string]any)["headers"].(map[string]any)["customRequestHeaders"].(map[string]any)["X-Garrison-Ingress"] != "abc123" ||
			ann["traefik.ingress.kubernetes.io/router.middlewares"] != "sso@file,app-todo-"+svc+"-ingress@kubernetescrd" {
			t.Errorf("%s: env %v, ingress %v, middleware %v", svc, envOf(private, svc), ing, mw)
		}
		rules := ing["spec"].(map[string]any)["rules"].([]any)
		if rules[0].(map[string]any)["host"] != host+".example.com" {
			t.Errorf("%s: rules %v", svc, rules)
		}
	}
	if envOf(private, "web")["MODE"] != "x" {
		t.Errorf("web lost its own environment: %v", envOf(private, "web"))
	}
	if _, has := envOf(private, "db")[IngressKeyEnv]; has {
		t.Error("an internal service got the key")
	}
	if private["Ingress/db"] != nil {
		t.Error("an internal service is routed")
	}
	public := render("dev")
	for name := range public {
		if strings.HasPrefix(name, "Middleware/") {
			t.Errorf("a public environment has %s", name)
		}
	}
	for _, svc := range []string{"web", "api", "db"} {
		if _, has := envOf(public, svc)[IngressKeyEnv]; has {
			t.Errorf("%s of a public environment got the key", svc)
		}
	}
	if public["Namespace/app-dev-todo"] == nil {
		t.Errorf("dev is not in its own namespace: %v", public)
	}
}

// A long stack name still makes a namespace name of at most 63 characters.
func TestNamespaceOf(t *testing.T) {
	if got := NamespaceOf("todo"); got != "app-todo" {
		t.Errorf("NamespaceOf(todo) = %q", got)
	}
	long := strings.Repeat("a", 63)
	if got := NamespaceOf(long); len(got) > 63 || got == NamespaceOf(long[:62]+"b") {
		t.Errorf("NamespaceOf(long) = %q", got)
	}
}

func TestMembersInEnv(t *testing.T) {
	s, err := stack.ParseIn([]byte("entrypoint: web\nservices:\n  web: { image: x/y:1, port: 80, env: { WRITERS: \"{{members}}\", OTHER: plain } }\nenvironments:\n  main: {}\n"), "media", "files")
	if err != nil {
		t.Fatal(err)
	}
	out, err := Kubernetes(s, Options{Environment: "main", Domain: "home.test", Members: []string{"alice", "bob"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "name: WRITERS\n              value: alice,bob") || !strings.Contains(string(out), "value: plain") {
		t.Errorf("%s", out)
	}
}
