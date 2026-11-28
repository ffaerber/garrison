package apply

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

// machines is a fake of two machines' pools and the ZFSVolumes naming them.
type machines struct {
	ds    map[string]map[string]bool // machine -> datasets
	owner string                     // the ZFSVolume's machine
	calls []string                   // "machine: command"
}

func newMachines() *machines {
	return &machines{ds: map[string]map[string]bool{
		"main":  {"garrison/ns": true, env: true, env + "/todo.todo-db": true},
		"node2": {"garrison/ns": true},
	}, owner: "main"}
}

func (m *machines) run(ctx context.Context, _ []string, _ []byte, name string, args ...string) ([]byte, error) {
	node := NodeOf(ctx)
	if node == "" {
		node = "main"
	}
	call := name + " " + strings.Join(args, " ")
	if name == "zfs" {
		m.calls = append(m.calls, node+": "+call)
	} else {
		m.calls = append(m.calls, call)
	}
	last := args[len(args)-1]
	pool := m.ds[node]
	switch {
	case strings.HasPrefix(call, "kubectl get zfsvolumes"):
		return []byte(fmt.Sprintf(`{"items":[{"metadata":{"name":"todo.todo-db"},"spec":{"ownerNodeID":%q}}]}`, m.owner)), nil
	case strings.HasPrefix(call, "kubectl get pods --namespace garrison --selector app=zfs"):
		return []byte("main node2"), nil
	case strings.HasPrefix(call, "kubectl patch zfsvolumes"):
		_, v, _ := strings.Cut(last, `"ownerNodeID":"`)
		m.owner = strings.TrimSuffix(v, `"}}`)
	case strings.HasPrefix(call, "zfs list"):
		if !pool[strings.Split(last, "@")[0]] {
			return nil, errors.New("does not exist")
		}
	case strings.HasPrefix(call, "zfs create -p"):
		pool[last] = true
	case strings.HasPrefix(call, "zfs destroy -r"):
		for d := range pool {
			if d == last || strings.HasPrefix(d, last+"/") {
				delete(pool, d)
			}
		}
	case strings.HasPrefix(call, "zfs rename"):
		old := args[len(args)-2]
		for d := range pool {
			if d == old || strings.HasPrefix(d, old+"/") {
				delete(pool, d)
				pool[last+strings.TrimPrefix(d, old)] = true
			}
		}
	}
	return nil, nil
}

// pipe copies the datasets below what send names, as zfs send -R and
// receive do, unless fail.
func (m *machines) pipe(fail bool) Pipe {
	return func(_ context.Context, from, to string, send, recv []string) error {
		m.calls = append(m.calls, fmt.Sprintf("pipe %s -> %s: %s | %s", from, to, strings.Join(send, " "), strings.Join(recv, " ")))
		target := recv[len(recv)-1]
		m.ds[to][target] = true // a receive that fails leaves half a copy
		if fail {
			return errors.New("connection reset")
		}
		src := strings.Split(send[len(send)-1], "@")[0]
		for d := range m.ds[from] {
			if strings.HasPrefix(d, src+"/") {
				m.ds[to][target+strings.TrimPrefix(d, src)] = true
			}
		}
		return nil
	}
}

func TestMoveEnv(t *testing.T) {
	StopTimeout = 10 * time.Second
	m, s := newMachines(), todoStack(t)
	ctx := context.Background()
	moved, err := MoveEnv(ctx, m.run, m.pipe(false), "", s, "main", "node2", "20261128-120000")
	if err != nil || !moved {
		t.Fatalf("moved %v: %v\n%s", moved, err, strings.Join(m.calls, "\n"))
	}
	// Stopped, then snapshotted and sent; its volume now names node2.
	stop := slices.Index(m.calls, "kubectl scale deployment --all --replicas 0 --namespace app-todo")
	snap := slices.Index(m.calls, "main: zfs snapshot -r "+env+"@move-20261128-120000")
	send := slices.Index(m.calls, "pipe main -> node2: zfs send -R "+env+"@move-20261128-120000 | zfs receive -u "+env)
	if stop < 0 || snap < stop || send < snap {
		t.Errorf("order:\n%s", strings.Join(m.calls, "\n"))
	}
	if !m.ds["node2"][env+"/todo.todo-db"] || m.owner != "node2" {
		t.Errorf("on node2: %v, owner %s", m.ds["node2"], m.owner)
	}
	for _, c := range []string{"kubectl delete persistentvolumeclaim data-todo-db --namespace app-todo --ignore-not-found --wait",
		"kubectl delete persistentvolume todo.todo-db --ignore-not-found --wait"} {
		if !slices.Contains(m.calls, c) {
			t.Errorf("not called: %s", c)
		}
	}
	// The old copy is kept on main, out of the way.
	if m.ds["main"][env] || !m.ds["main"][env+".moved-20261128-120000/todo.todo-db"] {
		t.Errorf("on main: %v", m.ds["main"])
	}
	// There already: nothing to do.
	if moved, err := MoveEnv(ctx, m.run, m.pipe(false), "", s, "main", "node2", "x"); moved || err != nil {
		t.Errorf("again: %v %v", moved, err)
	}
}

// A copy that fails changes nothing: half a copy is destroyed, the volume
// stays where it was.
func TestMoveEnvThatFails(t *testing.T) {
	StopTimeout = 10 * time.Second
	m, s := newMachines(), todoStack(t)
	moved, err := MoveEnv(context.Background(), m.run, m.pipe(true), "", s, "main", "node2", "1")
	if moved || err == nil || !strings.Contains(err.Error(), "connection reset") {
		t.Fatalf("moved %v: %v", moved, err)
	}
	if m.ds["node2"][env] || m.owner != "main" || !m.ds["main"][env+"/todo.todo-db"] {
		t.Errorf("changed: node2 %v, owner %s, main %v", m.ds["node2"], m.owner, m.ds["main"])
	}
}

func TestMoveEnvRefuses(t *testing.T) {
	ctx := context.Background()
	m, s := newMachines(), todoStack(t)
	if _, err := MoveEnv(ctx, m.run, m.pipe(false), "", s, "main", "node3", "1"); err == nil || !strings.Contains(err.Error(), "no machine node3") {
		t.Errorf("an unknown machine: %v", err)
	}
	m.ds["node2"][env] = true
	if _, err := MoveEnv(ctx, m.run, m.pipe(false), "", s, "main", "node2", "1"); err == nil || !strings.Contains(err.Error(), "has data of") {
		t.Errorf("data there already: %v", err)
	}
	for _, c := range m.calls {
		if strings.Contains(c, "scale") || strings.Contains(c, "snapshot") || strings.HasPrefix(c, "pipe") {
			t.Errorf("changed something: %s", c)
		}
	}
}
