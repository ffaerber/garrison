package system

import (
	"context"
	"os/exec"
	"strings"
	"testing"
)

const dir = "testdata/system"

func TestDatasets(t *testing.T) {
	got, err := Datasets(dir + "/01-db/compose.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, " ") != "/homecloud/system/db /homecloud/system/db-backup" {
		t.Errorf("datasets = %v", got)
	}
}

// The installer computes the same hash in shell; they must agree, or every
// handover from installer to controller restarts the services.
func TestHashMatchesInstaller(t *testing.T) {
	got, err := Hash(dir + "/01-db")
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("sh", "-c", `export LC_ALL=C; cat "$0"/* | sha256sum | cut -c1-12`, dir+"/01-db").Output()
	if err != nil {
		t.Skip("no shell tools:", err)
	}
	if want := strings.TrimSpace(string(out)); got != want {
		t.Errorf("Hash = %s, installer computes %s", got, want)
	}
}

func TestDeploy(t *testing.T) {
	var calls []string
	run := func(_ context.Context, _ []string, _ []byte, name string, args ...string) ([]byte, error) {
		calls = append(calls, name+" "+strings.Join(args, " "))
		return nil, nil
	}
	if err := Deploy(context.Background(), run, Stack{Name: "db", Dir: dir + "/01-db"}, []string{"DOMAIN=example.com"}); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(calls, "\n")
	for _, want := range []string{
		"zfs list -H -o name homecloud/system/db",
		"zfs list -H -o name homecloud/system/db-backup",
		"docker stack deploy --detach=true --resolve-image never --compose-file testdata/system/01-db/compose.yaml db",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}
