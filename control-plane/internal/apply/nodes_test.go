package apply

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// NodeExec runs zfs and the like in the zfs pod of the machine the context
// names, or of the main one; anything else as it is.
func TestNodeExec(t *testing.T) {
	var calls []string
	pods := "main zfs-a\nnode-2 zfs-b\n"
	gone := ""
	run := func(_ context.Context, _ []string, _ []byte, name string, args ...string) ([]byte, error) {
		call := name + " " + strings.Join(args, " ")
		calls = append(calls, call)
		switch {
		case strings.HasPrefix(call, "kubectl get pods"):
			return []byte(pods), nil
		case strings.HasPrefix(call, "kubectl get nodes"):
			return []byte("main"), nil
		case gone != "" && strings.Contains(call, "exec --namespace garrison -i "+gone+" "):
			return nil, errors.New(`Error from server (NotFound): pods "` + gone + `" not found`)
		}
		return nil, nil
	}
	ex := NodeExec(run)
	ctx := context.Background()

	if _, err := ex(WithNode(ctx, "node-2"), nil, nil, "zfs", "create", "-p", "garrison/ns/x"); err != nil {
		t.Fatal(err)
	}
	if want := "kubectl exec --namespace garrison -i zfs-b -- nsenter -t 1 -m -u -i -n -- zfs create -p garrison/ns/x"; calls[len(calls)-1] != want {
		t.Errorf("on node-2: %s", calls[len(calls)-1])
	}
	if _, err := ex(ctx, nil, nil, "rsync", "-a", "a/", "b/"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(calls[len(calls)-1], "-i zfs-a -- nsenter") {
		t.Errorf("without a machine, not on the main one: %s", calls[len(calls)-1])
	}
	n := len(calls)
	if _, err := ex(ctx, nil, nil, "kubectl", "get", "x"); err != nil || calls[n] != "kubectl get x" {
		t.Errorf("kubectl went elsewhere: %v", calls[n:])
	}

	// The pod was replaced: found again, once.
	gone, pods = "zfs-b", "main zfs-a\nnode-2 zfs-c\n"
	if _, err := ex(WithNode(ctx, "node-2"), nil, nil, "zfs", "list"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(calls[len(calls)-1], "-i zfs-c --") {
		t.Errorf("the new pod is not used: %v", calls)
	}
	if _, err := ex(WithNode(ctx, "node-3"), nil, nil, "zfs", "list"); err == nil || !strings.Contains(err.Error(), "node-3") {
		t.Errorf("a machine without a zfs pod: %v", err)
	}
}
