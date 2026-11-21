package apply

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"garrison/control-plane/internal/render"
	"garrison/control-plane/internal/stack"
)

// Snapshot prefixes. Sanoid's own snapshots start with autosnap_.
const (
	DeploySnapshot  = "deploy-"  // taken by the deployer before it redeploys an environment
	RestoreSnapshot = "restore-" // the state an environment had before a restore
)

// How many of the deployer's snapshots an environment keeps; Sanoid prunes its own.
const (
	KeepDeploys  = 10
	KeepRestores = 5
)

// RestoredProperty is the ZFS user property on an environment's dataset that
// names the last restore request carried out, so each runs once.
const RestoredProperty = "garrison:restored"

// EnvDataset is the ZFS dataset holding all datasets of an environment:
// <pool>/ns/<unit>/stacks/<hostname>.
func EnvDataset(root string, s *stack.Stack, env string) string {
	if root == "" {
		root = render.DefaultDataRoot
	}
	return render.EnvParent(root, s, env)
}

// onEnv names, in ctx, the machine that holds an environment's data, for
// the zfs and rsync commands that follow (WithNode).
func onEnv(ctx context.Context, run Runner, s *stack.Stack, env string) (context.Context, error) {
	node, err := EnvNode(ctx, run, render.StackName(s, env))
	if err != nil {
		return ctx, err
	}
	return WithNode(ctx, node), nil
}

// SnapshotStamp names a snapshot by its time, in UTC: 20261113-150405.
func SnapshotStamp(t time.Time) string { return t.UTC().Format("20060102-150405") }

func exists(ctx context.Context, run Runner, name string) bool {
	_, err := run(ctx, nil, nil, "zfs", "list", "-H", "-o", "name", name)
	return err == nil
}

// SnapshotEnv takes a recursive snapshot of an environment's datasets and
// prunes the oldest of the same kind. It does nothing for an environment
// without datasets and tells whether it took one.
func SnapshotEnv(ctx context.Context, run Runner, root string, s *stack.Stack, env, name string) (bool, error) {
	ctx, err := onEnv(ctx, run, s, env)
	if err != nil {
		return false, err
	}
	ds := EnvDataset(root, s, env)
	if !exists(ctx, run, ds) {
		return false, nil
	}
	if _, err := run(ctx, nil, nil, "zfs", "snapshot", "-r", ds+"@"+name); err != nil {
		return false, err
	}
	for prefix, keep := range map[string]int{DeploySnapshot: KeepDeploys, RestoreSnapshot: KeepRestores} {
		if strings.HasPrefix(name, prefix) {
			return true, prune(ctx, run, ds, prefix, keep)
		}
	}
	return true, nil
}

// prune destroys the snapshots of ds named prefix… but the newest keep.
func prune(ctx context.Context, run Runner, ds, prefix string, keep int) error {
	snaps, err := listSnapshots(ctx, run, ds, 1)
	if err != nil {
		return err
	}
	var mine []Snap
	for _, s := range snaps {
		if strings.HasPrefix(s.Name, prefix) {
			mine = append(mine, s)
		}
	}
	for len(mine) > keep {
		if _, err := run(ctx, nil, nil, "zfs", "destroy", "-r", ds+"@"+mine[0].Name); err != nil {
			return err
		}
		mine = mine[1:]
	}
	return nil
}

// Snap is one snapshot of an environment.
type Snap struct {
	Name    string    `json:"name"`
	Created time.Time `json:"created"`
}

// listSnapshots lists the snapshots of ds, oldest first, by dataset: depth
// 1 for ds's own, more to include its children's.
func listSnapshots(ctx context.Context, run Runner, ds string, depth int) ([]Snap, error) {
	m, err := ListSnapshots(ctx, run, ds, depth)
	return m[ds], err
}

// ListSnapshots lists the snapshots under ds, down to depth, by dataset,
// oldest first.
func ListSnapshots(ctx context.Context, run Runner, ds string, depth int) (map[string][]Snap, error) {
	out, err := run(ctx, nil, nil, "zfs", "list", "-H", "-p", "-t", "snapshot", "-o", "name,creation", "-s", "creation", "-d", strconv.Itoa(depth), ds)
	if err != nil {
		return nil, err
	}
	m := map[string][]Snap{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		full, created, ok := strings.Cut(line, "\t")
		dataset, name, ok2 := strings.Cut(full, "@")
		if !ok || !ok2 {
			continue
		}
		secs, _ := strconv.ParseInt(strings.TrimSpace(created), 10, 64)
		m[dataset] = append(m[dataset], Snap{Name: name, Created: time.Unix(secs, 0).UTC()})
	}
	return m, nil
}

// Restored is the restore request last carried out on an environment, or "".
func Restored(ctx context.Context, run Runner, root string, s *stack.Stack, env string) string {
	ctx, err := onEnv(ctx, run, s, env)
	if err != nil {
		return ""
	}
	out, err := run(ctx, nil, nil, "zfs", "get", "-H", "-o", "value", RestoredProperty, EnvDataset(root, s, env))
	if err != nil {
		return ""
	}
	if v := strings.TrimSpace(string(out)); v != "-" {
		return v
	}
	return ""
}

// StopTimeout is how long RestoreEnv waits for an environment's containers to stop.
var StopTimeout = 2 * time.Minute

// RestoreEnv brings the datasets of an environment back to a snapshot: it
// snapshots the current state (restore-<stamp>), stops the environment's
// services and copies each dataset back from the snapshot. Later snapshots
// are kept, so the restore can be undone. Shares are not restored: they
// belong to the whole unit. The caller deploys the environment again
// afterwards, which starts its services, whether this succeeded or not; id
// is recorded on success.
func RestoreEnv(ctx context.Context, run Runner, root string, s *stack.Stack, env, snapshot, stamp, id string) error {
	if root == "" {
		root = render.DefaultDataRoot
	}
	ctx, err := onEnv(ctx, run, s, env)
	if err != nil {
		return err
	}
	ds := EnvDataset(root, s, env)
	if !exists(ctx, run, ds+"@"+snapshot) {
		return fmt.Errorf("%s has no snapshot %s", s.Hostname(env), snapshot)
	}
	var datasets []string
	for _, name := range s.ServiceNames() {
		for _, v := range s.Services[name].Volumes {
			if v.Dataset == "" {
				continue
			}
			d := render.VolumeDataset(root, s, env, v.Dataset)
			if !exists(ctx, run, d+"@"+snapshot) {
				return fmt.Errorf("the dataset %s did not exist at %s; nothing was changed", v.Dataset, snapshot)
			}
			if !slices.Contains(datasets, d) {
				datasets = append(datasets, d)
			}
		}
	}
	if _, err := SnapshotEnv(ctx, run, root, s, env, RestoreSnapshot+stamp); err != nil {
		return err
	}
	if err := stopStack(ctx, run, render.StackName(s, env)); err != nil {
		return err
	}
	for _, d := range datasets {
		if err := copyBack(ctx, run, d, snapshot); err != nil {
			return err
		}
	}
	_, err = run(ctx, nil, nil, "zfs", "set", RestoredProperty+"="+id, ds)
	return err
}

// copyBack copies a dataset's files back from its snapshot, on its machine.
// OpenEBS mounts a dataset only into its pods (mountpoint legacy), and a pod
// sees no snapshots; so it is mounted on the machine for the copy, through
// ZFS's hidden .zfs directory, and unmounted again. --delete removes what
// was written since.
func copyBack(ctx context.Context, run Runner, ds, snapshot string) error {
	dir := volumeMount(ds)
	if _, err := run(ctx, nil, nil, "mkdir", "-p", dir); err != nil {
		return err
	}
	if _, err := run(ctx, nil, nil, "mount", "-t", "zfs", ds, dir); err != nil {
		return err
	}
	_, err := run(ctx, nil, nil, "rsync", "-aH", "--numeric-ids", "--delete", dir+"/.zfs/snapshot/"+snapshot+"/", dir+"/")
	if _, uerr := run(ctx, nil, nil, "umount", dir); err == nil {
		err = uerr
	}
	return err
}

// stopStack scales an environment's Deployments to nothing and waits until
// their pods are gone. Applying the stack again brings them back.
func stopStack(ctx context.Context, run Runner, name string) error {
	ns := render.NamespaceOf(name)
	if _, err := run(ctx, nil, nil, "kubectl", "scale", "deployment", "--all", "--replicas", "0", "--namespace", ns); err != nil {
		return err
	}
	deadline := time.Now().Add(StopTimeout)
	for {
		out, err := run(ctx, nil, nil, "kubectl", "get", "pods", "--namespace", ns, "--output", "name")
		if err == nil && strings.TrimSpace(string(out)) == "" {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the pods of %s did not stop within %s", name, StopTimeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}
