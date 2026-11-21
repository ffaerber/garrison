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

// fake records commands and pretends datasets in `have` exist; secrets
// answers `kubectl get secret NAME` with what kubectl prints for it.
type fake struct {
	have    map[string]bool
	secrets map[string]string
	calls   []string
	stdin   map[string]string // the last stdin of each command
	stdins  []string          // every command's stdin, in order
}

func (f *fake) run(_ context.Context, _ []string, stdin []byte, name string, args ...string) ([]byte, error) {
	call := name + " " + strings.Join(args, " ")
	f.calls = append(f.calls, call)
	if stdin != nil {
		f.stdin[call] = string(stdin)
	}
	f.stdins = append(f.stdins, string(stdin))
	switch {
	case strings.HasPrefix(call, "zfs list"):
		if !f.have[args[len(args)-1]] {
			return nil, errors.New("not found")
		}
	case strings.HasPrefix(call, "kubectl get secret registry-pull"):
		return []byte(`{"apiVersion":"v1","kind":"Secret","type":"kubernetes.io/dockerconfigjson","metadata":{"name":"registry-pull","namespace":"garrison","uid":"x"},"data":{".dockerconfigjson":"e30="}}`), nil
	case strings.HasPrefix(call, "kubectl get secret "):
		return []byte(f.secrets[args[2]]), nil
	}
	return nil, nil
}

func jellyfin(t *testing.T) *stack.Stack {
	t.Helper()
	data, err := os.ReadFile("../stack/testdata/jellyfin.yaml")
	if err != nil {
		t.Fatal(err)
	}
	s, err := stack.Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// called is the stdin of the first call that begins with prefix.
func (f *fake) called(prefix string) (string, bool) {
	for i, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			return f.stdins[i], true
		}
	}
	return "", false
}

func TestStack(t *testing.T) {
	s := jellyfin(t)
	f := &fake{have: map[string]bool{"garrison/ns/media/shares/media-library": true}, secrets: map[string]string{}, stdin: map[string]string{}}
	if err := Stack(context.Background(), f.run, s, render.Options{Environment: "dev", Domain: "example.com"}); err != nil {
		t.Fatal(err)
	}

	got := strings.Join(f.calls, "\n")
	for _, want := range []string{
		"zfs create -p garrison/ns/media/stacks/dev-jellyfin/jellyfin-config",
		"zfs set quota=5G garrison/ns/media/stacks/dev-jellyfin/jellyfin-config",
		"kubectl create --filename -",
		"kubectl apply --namespace app-dev-jellyfin --filename - --prune --selector garrison.io/stack=dev-jellyfin",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing command %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "zfs create -p garrison/ns/media/shares/media-library") {
		t.Error("existing share was created again")
	}
	deploy := strings.Index(got, "kubectl apply --namespace app-dev-jellyfin")
	if deploy < strings.LastIndex(got, "zfs ") || deploy < strings.LastIndex(got, "kubectl create") {
		t.Errorf("the stack must be applied after the datasets and secrets:\n%s", got)
	}
	ns, _ := f.called("kubectl apply --filename -")
	if !strings.Contains(ns, `"kind":"Namespace"`) || !strings.Contains(ns, `"name":"app-dev-jellyfin"`) {
		t.Errorf("the namespace is not made first: %s", ns)
	}
	if pull, _ := f.stdin["kubectl apply --filename -"]; !strings.Contains(got, "kubectl get secret registry-pull --namespace garrison") || pull == "" {
		t.Errorf("the pull secret is not copied:\n%s", got)
	}
	created, _ := f.called("kubectl create --filename -")
	if !strings.Contains(created, `"name":"jellyfin-api-key","namespace":"app-dev-jellyfin"`) {
		t.Errorf("generated secret: %s", created)
	}
	for _, c := range f.calls {
		if strings.HasPrefix(c, "kubectl create") && strings.Contains(f.stdin[c], "jellyfin-api-key") {
			if i := strings.Index(f.stdin[c], `"stringData":{"jellyfin-api-key":"`); i < 0 || len(f.stdin[c]) < i+35+64 {
				t.Errorf("generated secret has no 64-character value: %s", f.stdin[c])
			}
		}
	}
}

// A stored secret's value goes into its Secret, with its version; the pods
// carry the version, so a new one restarts them, and the manifests never
// hold the value.
func TestStackWithAStoredSecret(t *testing.T) {
	s := jellyfin(t)
	f := &fake{have: map[string]bool{}, secrets: map[string]string{"jellyfin-api-key": "v1"}, stdin: map[string]string{}}
	o := render.Options{Environment: "main", Domain: "example.com", Secrets: map[string]render.Stored{"jellyfin-api-key": {Value: "the real key", Version: "v2"}}}
	if err := Stack(context.Background(), f.run, s, o); err != nil {
		t.Fatal(err)
	}
	stored := ""
	for _, c := range f.calls {
		if strings.HasPrefix(c, "kubectl apply --filename -") && strings.Contains(f.stdin[c], "the real key") {
			stored = f.stdin[c]
		}
	}
	if !strings.Contains(stored, `"garrison.io/version":"v2"`) || !strings.Contains(stored, `"garrison.stored":"true"`) {
		t.Errorf("stored value not put into its Secret with its version: %q", stored)
	}
	if _, made := f.called("kubectl create --filename -"); made {
		t.Error("a generated value was made too")
	}
	manifests, err := render.Kubernetes(s, o)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(manifests), "garrison.io/secret-jellyfin-api-key: v2") || strings.Contains(string(manifests), "the real key") {
		t.Errorf("the pods must carry the version, and never the value:\n%s", manifests)
	}

	// The same version again: the Secret is left as it is.
	f2 := &fake{have: map[string]bool{}, secrets: map[string]string{"jellyfin-api-key": "v2"}, stdin: map[string]string{}}
	if err := Stack(context.Background(), f2.run, s, o); err != nil {
		t.Fatal(err)
	}
	for _, c := range f2.calls {
		if strings.Contains(f2.stdin[c], "the real key") {
			t.Errorf("an unchanged stored secret was written again: %s", c)
		}
	}
}

// A secret that was stored and is gone from the config gets a generated value again.
func TestAStoredSecretGoesBackToAGeneratedOne(t *testing.T) {
	s := jellyfin(t)
	f := &fake{have: map[string]bool{}, secrets: map[string]string{"jellyfin-api-key": "jellyfin-api-key true"}, stdin: map[string]string{}}
	if err := Stack(context.Background(), f.run, s, render.Options{Environment: "main", Domain: "example.com"}); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(f.calls, "\n")
	if !strings.Contains(got, "kubectl delete secret jellyfin-api-key --namespace app-jellyfin") || !strings.Contains(got, "kubectl create --filename -") {
		t.Errorf("the stored secret was not replaced:\n%s", got)
	}
}

func TestRemoveDeletesTheNamespace(t *testing.T) {
	f := &fake{stdin: map[string]string{}}
	if err := Remove(context.Background(), f.run, "dev-todo"); err != nil {
		t.Fatal(err)
	}
	if f.calls[0] != "kubectl delete namespace app-dev-todo --ignore-not-found --wait=false" {
		t.Errorf("%v", f.calls)
	}
}
