package apply

import (
	"context"
	"fmt"
	"strings"
	"sync"
)

// The machines of an installation: the main one, k3s's server, holds the
// platform; others joined it (installer/join.sh). An environment's data is
// on one of them, in its ZFS pool. Commands on data (zfs, rsync, mount) run
// on that machine, which the context names (WithNode), through the zfs
// DaemonSet's pod there (garrison/openebs); without a name, on the main one.

// MainNodeLabel is the label k3s gives its server, the main machine.
const MainNodeLabel = "node-role.kubernetes.io/control-plane"

// ZFSPods selects the pods of the DaemonSet zfs, one per machine, in the
// namespace garrison.
const ZFSPods = "app=zfs"

type nodeKey struct{}

// WithNode names the machine the commands on data run on.
func WithNode(ctx context.Context, node string) context.Context {
	return context.WithValue(ctx, nodeKey{}, node)
}

// NodeOf is the machine WithNode named, or "": the main one.
func NodeOf(ctx context.Context) string {
	n, _ := ctx.Value(nodeKey{}).(string)
	return n
}

// onHost are the commands that run on the machine of the data.
var onHost = map[string]bool{"zfs": true, "rsync": true, "mount": true, "umount": true, "mkdir": true}

// NodeExec returns a Runner that runs zfs, rsync, mount, umount and mkdir on
// the machine the context names (WithNode), or on the main one, in the host's
// namespaces: `kubectl exec` into the zfs pod there, then nsenter into the
// host's first process. Everything else runs with run.
func NodeExec(run Runner) Runner {
	var mu sync.Mutex
	pods := map[string]string{} // by node, "" the main one
	pod := func(ctx context.Context, node string) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		if p := pods[node]; p != "" {
			return p, nil
		}
		args := []string{"get", "pods", "--namespace", "garrison", "--selector", ZFSPods,
			"--field-selector", "status.phase=Running", "--output", "jsonpath={range .items[*]}{.spec.nodeName} {.metadata.name}{\"\\n\"}{end}"}
		out, err := run(ctx, nil, nil, "kubectl", args...)
		if err != nil {
			return "", err
		}
		want := node
		if want == "" {
			if want, err = MainNode(ctx, run); err != nil {
				return "", err
			}
		}
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			if n, p, ok := strings.Cut(line, " "); ok && n == want {
				pods[node] = p
				return p, nil
			}
		}
		return "", fmt.Errorf("no zfs pod runs on the machine %s", want)
	}
	return func(ctx context.Context, env []string, stdin []byte, name string, args ...string) ([]byte, error) {
		if !onHost[name] {
			return run(ctx, env, stdin, name, args...)
		}
		node := NodeOf(ctx)
		var out []byte
		var err error
		for try := 0; try < 2; try++ {
			var p string
			if p, err = pod(ctx, node); err != nil {
				return nil, err
			}
			out, err = run(ctx, env, stdin, "kubectl", append([]string{"exec", "--namespace", "garrison", "-i", p, "--",
				"nsenter", "-t", "1", "-m", "-u", "-i", "-n", "--", name}, args...)...)
			// A pod replaced meanwhile: find it again, once.
			if err == nil || !strings.Contains(err.Error(), "NotFound") {
				break
			}
			mu.Lock()
			delete(pods, node)
			mu.Unlock()
		}
		return out, err
	}
}

// MainNode is the name of the main machine, k3s's server.
func MainNode(ctx context.Context, run Runner) (string, error) {
	out, err := run(ctx, nil, nil, "kubectl", "get", "nodes", "--selector", MainNodeLabel, "--output", "jsonpath={.items[0].metadata.name}")
	if err != nil {
		return "", err
	}
	if n := strings.TrimSpace(string(out)); n != "" {
		return n, nil
	}
	return "", fmt.Errorf("no machine is labelled %s", MainNodeLabel)
}

// MainAddress is the main machine's own address on the pod network, the
// first of its pod range, as a CIDR of one address (10.42.0.0/32): where its
// host's traffic, Traefik's, comes from to pods on other machines. "" if
// k3s does not say (render.Options.MainAddress).
func MainAddress(ctx context.Context, run Runner) string {
	out, err := run(ctx, nil, nil, "kubectl", "get", "nodes", "--selector", MainNodeLabel, "--output", "jsonpath={.items[0].spec.podCIDR}")
	if err != nil {
		return ""
	}
	ip, _, ok := strings.Cut(strings.TrimSpace(string(out)), "/")
	if !ok || strings.Count(ip, ".") != 3 {
		return ""
	}
	return ip + "/32"
}

// Nodes are the machines with a zfs pod, which hold data.
func Nodes(ctx context.Context, run Runner) ([]string, error) {
	out, err := run(ctx, nil, nil, "kubectl", "get", "pods", "--namespace", "garrison", "--selector", ZFSPods, "--output", "jsonpath={.items[*].spec.nodeName}")
	if err != nil {
		return nil, err
	}
	return strings.Fields(string(out)), nil
}
