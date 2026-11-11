package apply

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"homecloud/controller/internal/render"
	"homecloud/controller/internal/stack"
)

// fake records commands and pretends datasets and secrets in `have` exist.
type fake struct {
	have  map[string]bool
	calls []string
	stdin map[string]string
}

func (f *fake) run(_ context.Context, _ []string, stdin []byte, name string, args ...string) ([]byte, error) {
	call := name + " " + strings.Join(args, " ")
	f.calls = append(f.calls, call)
	if stdin != nil {
		f.stdin[call] = string(stdin)
	}
	last := args[len(args)-1]
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
	f := &fake{have: map[string]bool{"homecloud/ns/media/shares/media-library": true}, stdin: map[string]string{}}
	if err := Stack(context.Background(), f.run, s, render.Options{Environment: "dev", Domain: "example.com"}); err != nil {
		t.Fatal(err)
	}

	got := strings.Join(f.calls, "\n")
	for _, want := range []string{
		"zfs create -p homecloud/ns/media/stacks/dev-jellyfin/jellyfin-config",
		"zfs set quota=5G homecloud/ns/media/stacks/dev-jellyfin/jellyfin-config",
		"docker secret create dev-jellyfin.jellyfin-api-key -",
		"docker stack deploy --detach=true --resolve-image never --compose-file - dev-jellyfin",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing command %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "zfs create -p homecloud/ns/media/shares/media-library") {
		t.Error("existing share was created again")
	}
	if !strings.HasSuffix(got, "dev-jellyfin") {
		t.Errorf("deploy must come last:\n%s", got)
	}
	if v := f.stdin["docker secret create dev-jellyfin.jellyfin-api-key -"]; len(v) != 64 {
		t.Errorf("generated secret has length %d, want 64", len(v))
	}
}
