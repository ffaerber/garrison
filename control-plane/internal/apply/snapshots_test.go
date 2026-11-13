package apply

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"homecloud/control-plane/internal/stack"
)

// zfsFake keeps datasets and their snapshots, and the containers of stacks.
type zfsFake struct {
	datasets map[string]bool
	snaps    map[string][]string // dataset -> snapshot names, oldest first
	props    map[string]string
	running  int // containers left; each docker ps sees one fewer
	calls    []string
}

func (f *zfsFake) run(_ context.Context, _ []string, _ []byte, name string, args ...string) ([]byte, error) {
	call := name + " " + strings.Join(args, " ")
	f.calls = append(f.calls, call)
	last := args[len(args)-1]
	switch {
	case strings.HasPrefix(call, "zfs list -H -p -t snapshot"):
		var b strings.Builder
		for i, s := range f.snaps[last] {
			fmt.Fprintf(&b, "%s@%s\t%d\n", last, s, 1700000000+i)
		}
		return []byte(b.String()), nil
	case strings.HasPrefix(call, "zfs list"):
		ds, snap, ok := strings.Cut(last, "@")
		if !f.datasets[ds] || ok && !slices.Contains(f.snaps[ds], snap) {
			return nil, errors.New("does not exist")
		}
	case strings.HasPrefix(call, "zfs snapshot -r"):
		ds, snap, _ := strings.Cut(last, "@")
		for d := range f.datasets {
			if d == ds || strings.HasPrefix(d, ds+"/") {
				f.snaps[d] = append(f.snaps[d], snap)
			}
		}
	case strings.HasPrefix(call, "zfs destroy -r"):
		ds, snap, _ := strings.Cut(last, "@")
		for d := range f.datasets {
			if d == ds || strings.HasPrefix(d, ds+"/") {
				f.snaps[d] = slices.DeleteFunc(f.snaps[d], func(s string) bool { return s == snap })
			}
		}
	case strings.HasPrefix(call, "zfs set"):
		k, v, _ := strings.Cut(args[1], "=")
		f.props[last+" "+k] = v
	case strings.HasPrefix(call, "zfs get"):
		if v, ok := f.props[last+" "+args[4]]; ok {
			return []byte(v + "\n"), nil
		}
		return []byte("-\n"), nil
	case strings.HasPrefix(call, "docker service ls"):
		return []byte("svc1\nsvc2\n"), nil
	case strings.HasPrefix(call, "docker ps"):
		if f.running > 0 {
			f.running--
			return []byte("c1\n"), nil
		}
	}
	return nil, nil
}

func todoStack(t *testing.T) *stack.Stack {
	s, err := stack.ParseIn([]byte(`entrypoint: web
services:
  web: { image: "todo:1", port: 8080 }
  db:
    image: postgres:17-alpine
    volumes: { data: { dataset: todo-db, size: 2Gi, path: /var/lib/postgresql/data } }
environments:
  main: {}
`), "homepage", "todo")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

const env = "homecloud/ns/homepage/stacks/todo"

func newZFS() *zfsFake {
	return &zfsFake{
		datasets: map[string]bool{env: true, env + "/todo-db": true},
		snaps:    map[string][]string{},
		props:    map[string]string{},
	}
}

func TestSnapshotEnvKeepsTheNewest(t *testing.T) {
	f, s := newZFS(), todoStack(t)
	for i := range KeepDeploys + 3 {
		took, err := SnapshotEnv(context.Background(), f.run, "", s, "main", fmt.Sprintf("%s%02d", DeploySnapshot, i))
		if err != nil || !took {
			t.Fatal(took, err)
		}
	}
	got := f.snaps[env]
	if len(got) != KeepDeploys || got[0] != DeploySnapshot+"03" || f.snaps[env+"/todo-db"][0] != DeploySnapshot+"03" {
		t.Errorf("snapshots = %v, children %v", got, f.snaps[env+"/todo-db"])
	}
	// Sanoid's are not the deployer's to prune.
	f.snaps[env] = append([]string{"autosnap_old"}, f.snaps[env]...)
	SnapshotEnv(context.Background(), f.run, "", s, "main", DeploySnapshot+"99")
	if f.snaps[env][0] != "autosnap_old" {
		t.Errorf("pruned Sanoid's: %v", f.snaps[env])
	}
}

func TestSnapshotEnvWithoutData(t *testing.T) {
	f := &zfsFake{datasets: map[string]bool{}, snaps: map[string][]string{}}
	took, err := SnapshotEnv(context.Background(), f.run, "", todoStack(t), "dev", DeploySnapshot+"1")
	if took || err != nil {
		t.Errorf("took = %v, err = %v", took, err)
	}
}

func TestRestoreEnv(t *testing.T) {
	StopTimeout = 10 * time.Second
	f, s := newZFS(), todoStack(t)
	f.snaps[env] = []string{"deploy-1"}
	f.snaps[env+"/todo-db"] = []string{"deploy-1"}
	f.running = 1
	ctx := context.Background()
	if err := RestoreEnv(ctx, f.run, "", s, "main", "deploy-1", "20261113-150405", "c0ffee"); err != nil {
		t.Fatal(err)
	}
	// The state before is kept, the services stopped before copying, and
	// the request recorded.
	if !slices.Contains(f.snaps[env+"/todo-db"], RestoreSnapshot+"20261113-150405") {
		t.Errorf("no snapshot of the state before: %v", f.snaps)
	}
	scale := slices.Index(f.calls, "docker service scale --detach svc1=0 svc2=0")
	copy := slices.Index(f.calls, "rsync -aH --numeric-ids --delete /homecloud/ns/homepage/stacks/todo/todo-db/.zfs/snapshot/deploy-1/ /homecloud/ns/homepage/stacks/todo/todo-db/")
	if scale < 0 || copy < scale {
		t.Errorf("calls = %v", f.calls)
	}
	if got := Restored(ctx, f.run, "", s, "main"); got != "c0ffee" {
		t.Errorf("Restored = %q", got)
	}

	// A snapshot that does not exist changes nothing.
	f.calls = nil
	if err := RestoreEnv(ctx, f.run, "", s, "main", "deploy-2", "x", "d00d"); err == nil || !strings.Contains(err.Error(), "no snapshot deploy-2") {
		t.Errorf("err = %v", err)
	}
	for _, c := range f.calls {
		if !strings.HasPrefix(c, "zfs list") {
			t.Errorf("changed something: %s", c)
		}
	}
}
