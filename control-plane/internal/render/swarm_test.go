package render

import (
	"flag"
	"os"
	"testing"

	"homecloud/control-plane/internal/stack"
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
