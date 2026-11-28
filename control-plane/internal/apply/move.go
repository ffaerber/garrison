package apply

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path"
	"slices"
	"strings"

	"garrison/control-plane/internal/render"
	"garrison/control-plane/internal/stack"
)

// Moving an environment to another machine (docs/storage.md, Moving an app):
// its services stop, its datasets go to the other machine in one stream
// (zfs send -R, with their properties and snapshots), its volumes are
// pointed there (the ZFSVolume's owner; the PersistentVolumes and claims
// made again, as a PV's machine cannot change), and applying the stack
// starts it there. The old copy stays on the old machine, renamed, as a way
// back.
//
// This first version stops the environment for the whole copy, and the
// stream goes through the Kubernetes API (kubectl exec on each machine's
// zfs pod, joined by the deployer): right for small data; large data waits
// for the transfer between the machines' zfs pods themselves.

// MoveSnapshot names the snapshot a move sends.
const MoveSnapshot = "move-"

// MovedSuffix is added to an environment's old dataset after a move.
const MovedSuffix = ".moved-"

// Pipe runs send on the machine from and recv on the machine to, with
// send's output as recv's input.
type Pipe func(ctx context.Context, from, to string, send, recv []string) error

// zfsPod is the zfs DaemonSet's pod on node.
func zfsPod(ctx context.Context, run Runner, node string) (string, error) {
	out, err := run(ctx, nil, nil, "kubectl", "get", "pods", "--namespace", "garrison", "--selector", ZFSPods,
		"--field-selector", "spec.nodeName="+node+",status.phase=Running", "--output", "jsonpath={.items[0].metadata.name}")
	if err != nil {
		return "", err
	}
	if p := strings.TrimSpace(string(out)); p != "" {
		return p, nil
	}
	return "", fmt.Errorf("no zfs pod runs on the machine %s", node)
}

// NodePipe is the Pipe through each machine's zfs pod, in the host's
// namespaces, as NodeExec runs commands; run finds the pods.
func NodePipe(run Runner) Pipe {
	return func(ctx context.Context, from, to string, send, recv []string) error {
		pf, err := zfsPod(ctx, run, from)
		if err != nil {
			return err
		}
		pt, err := zfsPod(ctx, run, to)
		if err != nil {
			return err
		}
		host := []string{"--", "nsenter", "-t", "1", "-m", "-u", "-i", "-n", "--"}
		src := exec.CommandContext(ctx, "kubectl", append(append([]string{"exec", "--namespace", "garrison", pf}, host...), send...)...)
		dst := exec.CommandContext(ctx, "kubectl", append(append([]string{"exec", "--namespace", "garrison", "-i", pt}, host...), recv...)...)
		var srcErr, dstErr bytes.Buffer
		src.Stderr, dst.Stderr = &srcErr, &dstErr
		out, err := src.StdoutPipe()
		if err != nil {
			return err
		}
		dst.Stdin = out
		if err := src.Start(); err != nil {
			return err
		}
		if err := dst.Start(); err != nil {
			src.Process.Kill()
			src.Wait()
			return err
		}
		errDst := dst.Wait()
		if errDst != nil {
			src.Process.Kill()
		}
		errSrc := src.Wait()
		if errSrc != nil && errDst == nil {
			return fmt.Errorf("%s on %s: %v: %s", strings.Join(send, " "), from, errSrc, strings.TrimSpace(srcErr.String()))
		}
		if errDst != nil {
			return fmt.Errorf("%s on %s: %v: %s", strings.Join(recv, " "), to, errDst, strings.TrimSpace(dstErr.String()))
		}
		return nil
	}
}

// MoveEnv moves an environment's data to the machine to, stopping it first
// (see above). Applying the stack afterwards starts it there; on an error
// before its volumes were pointed there, it starts on the old machine
// again, as nothing changed. moved: whether its volumes now name to.
func MoveEnv(ctx context.Context, run Runner, pipe Pipe, root string, s *stack.Stack, env, to, stamp string) (moved bool, err error) {
	if root == "" {
		root = render.DefaultDataRoot
	}
	shares, datasets := volumeNames(s)
	if len(shares) > 0 {
		return false, fmt.Errorf("%s mounts the unit's shares (%s), which other stacks use too; it cannot move alone", s.Hostname(env), strings.Join(shares, ", "))
	}
	if len(datasets) == 0 {
		return false, fmt.Errorf("%s has no data; it runs wherever Kubernetes puts it", s.Hostname(env))
	}
	stackName := render.StackName(s, env)
	from, err := EnvNode(ctx, run, stackName)
	if err != nil {
		return false, err
	}
	if from == "" {
		return false, fmt.Errorf("%s has no volumes yet", s.Hostname(env))
	}
	if from == to {
		return false, nil
	}
	nodes, err := Nodes(ctx, run)
	if err != nil {
		return false, err
	}
	if !slices.Contains(nodes, to) {
		return false, fmt.Errorf("there is no machine %s with a pool; the machines are %s", to, strings.Join(nodes, ", "))
	}
	parent := EnvDataset(root, s, env)
	onFrom, onTo := WithNode(ctx, from), WithNode(ctx, to)
	if exists(onTo, run, parent) {
		return false, fmt.Errorf("%s has data of %s already (%s): move it away or destroy it first", to, s.Hostname(env), parent)
	}
	if err := stopStack(ctx, run, stackName); err != nil {
		return false, err
	}
	snap := parent + "@" + MoveSnapshot + stamp
	if _, err := run(onFrom, nil, nil, "zfs", "snapshot", "-r", snap); err != nil {
		return false, err
	}
	if err := EnsureDataset(onTo, run, "/"+path.Dir(parent), ""); err != nil {
		return false, err
	}
	if err := pipe(ctx, from, to, []string{"zfs", "send", "-R", snap}, []string{"zfs", "receive", "-u", parent}); err != nil {
		// Half a copy is no copy: gone, so that the next try starts clean.
		if exists(onTo, run, parent) {
			run(onTo, nil, nil, "zfs", "destroy", "-r", parent)
		}
		return false, fmt.Errorf("copying %s to %s: %w", parent, to, err)
	}

	// The volumes: their ZFSVolume now on to; their PV and claim made again
	// by applying the stack (ensureVolumes), as a PV's machine is fixed.
	ns := render.Namespace(s, env)
	names := make([]string, 0, len(datasets))
	for d := range datasets {
		names = append(names, d)
	}
	slices.Sort(names)
	var errs []error
	for _, d := range names {
		vol := render.VolumeName(s, env, d)
		if _, err := run(ctx, nil, nil, "kubectl", "delete", "persistentvolumeclaim", render.DataClaim(d), "--namespace", ns, "--ignore-not-found", "--wait"); err != nil {
			errs = append(errs, err)
			continue
		}
		if _, err := run(ctx, nil, nil, "kubectl", "delete", "persistentvolume", vol, "--ignore-not-found", "--wait"); err != nil {
			errs = append(errs, err)
			continue
		}
		if _, err := run(ctx, nil, nil, "kubectl", "patch", "zfsvolumes.zfs.openebs.io", vol, "--namespace", ZFSVolumes,
			"--type", "merge", "--patch", `{"spec":{"ownerNodeID":"`+to+`"}}`); err != nil {
			errs = append(errs, err)
		}
	}
	if err := errors.Join(errs...); err != nil {
		return true, fmt.Errorf("the data is on %s, but its volumes were not all pointed there: %w", to, err)
	}
	// The old copy, out of the way of a move back, kept as a way back.
	if _, err := run(onFrom, nil, nil, "zfs", "rename", parent, parent+MovedSuffix+stamp); err != nil {
		return true, fmt.Errorf("moved to %s; its old copy on %s could not be renamed: %w", to, from, err)
	}
	return true, nil
}
