package system

import (
	"context"
	"os/exec"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const dir = "testdata/system"

func render(t *testing.T, name string) []byte {
	t.Helper()
	out, err := Render(Stack{Name: name, Dir: dir + "/" + name}, []string{"DOMAIN=example.com"})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// Render fills in the variables it knows, labels every object with its
// stack, and puts the files a ConfigMap names into its data.
func TestRender(t *testing.T) {
	out := string(render(t, "01-db"))
	hash, err := Hash(dir + "/01-db")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"value: db.example.com",
		"value: ${NOT_SET}",
		"garrison.io/config-hash: " + hash,
		"garrison.io/stack: 01-db",
		"init.sql: |",
		"CREATE TABLE",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	dec := yaml.NewDecoder(strings.NewReader(out))
	n := 0
	for {
		var obj struct {
			Metadata struct {
				Labels map[string]string `yaml:"labels"`
			} `yaml:"metadata"`
		}
		if dec.Decode(&obj) != nil {
			break
		}
		n++
		if obj.Metadata.Labels[StackLabel] != "01-db" {
			t.Errorf("object %d is not labelled with its stack: %v", n, obj.Metadata.Labels)
		}
	}
	if n != 2 {
		t.Errorf("rendered %d objects, want 2", n)
	}
	// labels: with no value becomes a mapping holding the stack's label.
	if !strings.Contains(string(render(t, "02-web")), "garrison.io/stack: 02-web") {
		t.Error("an empty labels: is not filled in")
	}
}

func TestRenderRefusesFilesOutsideTheStack(t *testing.T) {
	if _, err := Render(Stack{Name: "x", Dir: t.TempDir()}, nil); err == nil {
		t.Error("a directory without kubernetes.yaml rendered")
	}
}

func TestDatasets(t *testing.T) {
	got, err := Datasets(render(t, "01-db"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, " ") != "/garrison/system/db /garrison/system/db-backup" {
		t.Errorf("datasets = %v", got)
	}
}

// Hash must stay what it was: config-hash annotations of running pods use it.
func TestHashIsTheFilesSha256(t *testing.T) {
	got, err := Hash(dir + "/01-db")
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("sh", "-c", `export LC_ALL=C; cat "$0"/* | sha256sum | cut -c1-12`, dir+"/01-db").Output()
	if err != nil {
		t.Skip("no shell tools:", err)
	}
	if want := strings.TrimSpace(string(out)); got != want {
		t.Errorf("Hash = %s, sha256sum says %s", got, want)
	}
}

func TestDeploy(t *testing.T) {
	var calls []string
	var applied string
	run := func(_ context.Context, _ []string, stdin []byte, name string, args ...string) ([]byte, error) {
		calls = append(calls, name+" "+strings.Join(args, " "))
		if name == "kubectl" {
			applied = string(stdin)
		}
		return nil, nil
	}
	if err := Deploy(context.Background(), run, Stack{Name: "01-db", Dir: dir + "/01-db"}, []string{"DOMAIN=example.com"}); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(calls, "\n")
	for _, want := range []string{
		"zfs list -H -o name garrison/system/db",
		"zfs list -H -o name garrison/system/db-backup",
		"kubectl apply --namespace garrison --filename - --prune --selector garrison.io/stack=01-db --prune-allowlist core/v1/ConfigMap",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if !strings.Contains(applied, "db.example.com") {
		t.Errorf("kubectl was not given the rendered stack:\n%s", applied)
	}
}

func TestWaitAndRemove(t *testing.T) {
	var calls []string
	run := func(_ context.Context, _ []string, _ []byte, name string, args ...string) ([]byte, error) {
		calls = append(calls, name+" "+strings.Join(args, " "))
		if args[0] == "get" {
			return []byte("deployment.apps/web\nstatefulset.apps/db\n"), nil
		}
		return nil, nil
	}
	if err := Wait(context.Background(), run, "db", "60s"); err != nil {
		t.Fatal(err)
	}
	if err := Remove(context.Background(), run, "db"); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(calls, "\n")
	for _, want := range []string{
		"kubectl rollout status deployment.apps/web --namespace garrison --timeout 60s",
		"kubectl rollout status statefulset.apps/db --namespace garrison --timeout 60s",
		"kubectl delete configmap,service,serviceaccount,persistentvolumeclaim,deployment,statefulset,daemonset,job.batch,ingress.networking.k8s.io,networkpolicy.networking.k8s.io,role.rbac.authorization.k8s.io",
		"--selector garrison.io/stack=db --ignore-not-found",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}
