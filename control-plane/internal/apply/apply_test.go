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
	have     map[string]bool
	secrets  map[string]string
	calls    []string
	stdin    map[string]string // the last stdin of each command
	stdins   []string          // every command's stdin, in order
	volumes  string            // the ZFSVolumes there are, as JSON objects
	capacity map[string]string // the capacity of each ZFSVolume there is
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
	case strings.HasPrefix(call, "kubectl get zfsvolumes.zfs.openebs.io --namespace"):
		return []byte(`{"items":[` + f.volumes + `]}`), nil
	case strings.HasPrefix(call, "kubectl get zfsvolumes.zfs.openebs.io "):
		if !strings.Contains(f.volumes, `"name":"`+args[2]+`"`) {
			return nil, errors.New("NotFound")
		}
		return []byte(f.capacity[args[2]]), nil
	case strings.HasPrefix(call, "kubectl get zfsnodes"):
		return []byte(`{"items":[{"metadata":{"name":"main"},"pools":[{"name":"garrison","free":"9Gi"}]},
 {"metadata":{"name":"node-2"},"pools":[{"name":"garrison","free":"31457280Ki"}]}]}`), nil
	case strings.HasPrefix(call, "kubectl get persistentvolume "):
		return []byte("Bound"), nil
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

// ofKind is the stdin of the first call that gave kubectl an object of kind.
func (f *fake) ofKind(kind string) string {
	if i := f.index(kind); i >= 0 {
		return f.stdins[i]
	}
	return ""
}

// index is the first call that gave kubectl an object of kind, or -1.
func (f *fake) index(kind string) int {
	for i := range f.calls {
		if strings.Contains(f.stdins[i], `{"apiVersion":"v1","kind":"`+kind+`"`) {
			return i
		}
	}
	return -1
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
		// The environment's dataset, under which OpenEBS makes its volumes.
		"zfs create -p garrison/ns/media/stacks/dev-jellyfin",
		// The share from before OpenEBS is adopted: renamed and made legacy.
		"zfs rename garrison/ns/media/shares/media-library garrison/ns/media/shares/media.shares.media-library",
		"zfs set mountpoint=legacy garrison/ns/media/shares/media.shares.media-library",
		"kubectl create --filename -",
		"kubectl apply --namespace app-dev-jellyfin --filename - --prune --selector garrison.io/stack=dev-jellyfin",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing command %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "zfs create -p garrison/ns/media/stacks/dev-jellyfin/") {
		t.Error("the deployer created a volume's dataset; OpenEBS does")
	}
	// Each volume a ZFSVolume on the machine with the most free space, and a
	// PersistentVolume there bound to its claim.
	var zv, pv []string
	for i, c := range f.calls {
		if strings.Contains(f.stdins[i], `"kind":"ZFSVolume"`) && c == "kubectl create --filename -" {
			zv = append(zv, f.stdins[i])
		}
		if strings.Contains(f.stdins[i], `"kind":"PersistentVolume"`) {
			pv = append(pv, f.stdins[i])
		}
	}
	if len(zv) != 2 || !strings.Contains(zv[0], `"name":"dev-jellyfin.jellyfin-config"`) || !strings.Contains(zv[0], `"ownerNodeID":"node-2"`) ||
		!strings.Contains(zv[0], `"poolName":"garrison/ns/media/stacks/dev-jellyfin"`) || !strings.Contains(zv[0], `"capacity":"5368709120"`) {
		t.Errorf("ZFSVolumes: %v", zv)
	}
	if !strings.Contains(zv[1], `"name":"media.shares.media-library"`) || !strings.Contains(zv[1], `"shared":"yes"`) {
		t.Errorf("the share's ZFSVolume: %s", zv[1])
	}
	if len(pv) != 2 || !strings.Contains(pv[0], `"claimRef":{"name":"data-jellyfin-config","namespace":"app-dev-jellyfin"}`) ||
		!strings.Contains(pv[0], `"values":["node-2"]`) || !strings.Contains(pv[1], `"name":"media.shares.media-library.app-dev-jellyfin"`) {
		t.Errorf("PersistentVolumes: %v", pv)
	}
	deploy := strings.Index(got, "kubectl apply --namespace app-dev-jellyfin")
	if deploy < strings.LastIndex(got, "zfs ") || deploy < strings.LastIndex(got, "kubectl create") {
		t.Errorf("the stack must be applied after the volumes and secrets:\n%s", got)
	}
	if ns := f.ofKind("Namespace"); !strings.Contains(ns, `"name":"app-dev-jellyfin"`) || f.index("Namespace") > f.index("Secret") {
		t.Errorf("the namespace is not made before the secrets: %s", ns)
	}
	if pull, _ := f.stdin["kubectl apply --filename -"]; !strings.Contains(got, "kubectl get secret registry-pull --namespace garrison") || pull == "" {
		t.Errorf("the pull secret is not copied:\n%s", got)
	}
	created := ""
	for i, c := range f.calls {
		if c == "kubectl create --filename -" && strings.Contains(f.stdins[i], `"kind":"Secret"`) {
			created = f.stdins[i]
		}
	}
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

// A volume whose size changed keeps its machine and gets the new size as its
// ZFSVolume's capacity and its dataset's quota, smaller too; one of the same
// size is left alone.
func TestAVolumeChangesItsSize(t *testing.T) {
	s := jellyfin(t)
	f := &fake{have: map[string]bool{}, secrets: map[string]string{}, stdin: map[string]string{},
		volumes:  `{"metadata":{"name":"dev-jellyfin.jellyfin-config"},"spec":{"ownerNodeID":"main"}},{"metadata":{"name":"media.shares.media-library"},"spec":{"ownerNodeID":"main"}}`,
		capacity: map[string]string{"dev-jellyfin.jellyfin-config": "1073741824", "media.shares.media-library": "1099511627776"}}
	if err := Stack(context.Background(), f.run, s, render.Options{Environment: "dev", Domain: "example.com"}); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(f.calls, "\n")
	for _, want := range []string{
		"zfs set quota=5G garrison/ns/media/stacks/dev-jellyfin/dev-jellyfin.jellyfin-config",
		`kubectl patch zfsvolumes.zfs.openebs.io dev-jellyfin.jellyfin-config --namespace garrison --type merge --patch {"spec":{"capacity":"5368709120"}}`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "media.shares.media-library --namespace garrison --type merge") {
		t.Error("the share, of the same size, was changed")
	}
	if pv := f.ofKind("PersistentVolume"); !strings.Contains(pv, `"capacity":{"storage":"5Gi"}`) || !strings.Contains(pv, `"values":["main"]`) {
		t.Errorf("the PersistentVolume: %s", pv)
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
	if f.ofKind("Secret") != "" && !strings.Contains(f.ofKind("Secret"), "the real key") {
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

func TestRemoveKeepsTheSecrets(t *testing.T) {
	f := &fake{stdin: map[string]string{}}
	if err := Remove(context.Background(), f.run, "dev-todo"); err != nil {
		t.Fatal(err)
	}
	if f.calls[0] != "kubectl delete deployments,services,ingresses,networkpolicies,middlewares.traefik.io --namespace app-dev-todo --all --ignore-not-found --wait=false" {
		t.Errorf("%v", f.calls)
	}
}
