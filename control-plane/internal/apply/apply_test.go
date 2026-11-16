package apply

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"garrison/control-plane/internal/render"
	"garrison/control-plane/internal/stack"
)

// fake records commands and pretends datasets and secrets in `have` exist.
type fake struct {
	have  map[string]bool
	calls []string
	stdin map[string]string
	ls    string // what `docker secret ls` answers
}

func (f *fake) run(_ context.Context, _ []string, stdin []byte, name string, args ...string) ([]byte, error) {
	call := name + " " + strings.Join(args, " ")
	f.calls = append(f.calls, call)
	if stdin != nil {
		f.stdin[call] = string(stdin)
	}
	last := args[len(args)-1]
	if strings.HasPrefix(call, "docker secret ls") {
		return []byte(f.ls), nil
	}
	if strings.HasPrefix(call, "zfs list") || strings.HasPrefix(call, "docker secret inspect") {
		if !f.have[last] {
			return nil, errors.New("not found")
		}
	}
	return nil, nil
}

func TestStack(t *testing.T) {
	data, err := os.ReadFile("../stack/testdata/jellyfin.yaml")
	if err != nil {
		t.Fatal(err)
	}
	s, err := stack.Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	f := &fake{have: map[string]bool{"garrison/ns/media/shares/media-library": true}, stdin: map[string]string{}}
	if err := Stack(context.Background(), f.run, s, render.Options{Environment: "dev", Domain: "example.com"}); err != nil {
		t.Fatal(err)
	}

	got := strings.Join(f.calls, "\n")
	for _, want := range []string{
		"zfs create -p garrison/ns/media/stacks/dev-jellyfin/jellyfin-config",
		"zfs set quota=5G garrison/ns/media/stacks/dev-jellyfin/jellyfin-config",
		"docker secret create dev-jellyfin.jellyfin-api-key -",
		"docker stack deploy --detach=true --resolve-image never --with-registry-auth --compose-file - dev-jellyfin",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing command %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "zfs create -p garrison/ns/media/shares/media-library") {
		t.Error("existing share was created again")
	}
	deploy := strings.Index(got, "docker stack deploy")
	if deploy < strings.LastIndex(got, "zfs ") || deploy < strings.LastIndex(got, "docker secret create") {
		t.Errorf("deploy must come after the datasets and secrets:\n%s", got)
	}
	if v := f.stdin["docker secret create dev-jellyfin.jellyfin-api-key -"]; len(v) != 64 {
		t.Errorf("generated secret has length %d, want 64", len(v))
	}
}

// A stored secret's value goes to a Swarm secret named by its version; the
// stack uses that one, and earlier versions are removed after the deploy.
func TestStackWithAStoredSecret(t *testing.T) {
	data, err := os.ReadFile("../stack/testdata/jellyfin.yaml")
	if err != nil {
		t.Fatal(err)
	}
	s, err := stack.Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	f := &fake{have: map[string]bool{}, stdin: map[string]string{},
		ls: "jellyfin.jellyfin-api-key.old1\njellyfin.jellyfin-api-key.v2\n"}
	o := render.Options{Environment: "main", Domain: "example.com", Secrets: map[string]render.Stored{"jellyfin-api-key": {Value: "the real key", Version: "v2"}}}
	if err := Stack(context.Background(), f.run, s, o); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(f.calls, "\n")
	create := "docker secret create --label garrison.stored=true jellyfin.jellyfin-api-key.v2 -"
	if f.stdin[create] != "the real key" {
		t.Errorf("stored value not given to Swarm:\n%s", got)
	}
	if strings.Contains(got, "docker secret create jellyfin.jellyfin-api-key -") {
		t.Error("a generated value was made too")
	}
	if !strings.Contains(got, "docker secret rm jellyfin.jellyfin-api-key.old1") || strings.Contains(got, "rm jellyfin.jellyfin-api-key.v2") {
		t.Errorf("earlier versions not pruned, or the current one was:\n%s", got)
	}
	compose, err := render.Swarm(s, o)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(compose), "name: jellyfin.jellyfin-api-key.v2") || strings.Contains(string(compose), "the real key") {
		t.Errorf("the stack must name the version, and never hold the value:\n%s", compose)
	}
}
