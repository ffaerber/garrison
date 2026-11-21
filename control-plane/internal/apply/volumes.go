package apply

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"sort"
	"strconv"
	"strings"

	"garrison/control-plane/internal/render"
	"garrison/control-plane/internal/stack"
)

// The volumes of an environment and the shares of a unit are ZFSVolumes of
// OpenEBS ZFS LocalPV, in the namespace garrison, which the deployer writes
// itself (static volumes): so it chooses the machine, and the datasets keep
// Garrison's layout (render, "An environment's volume and a unit's share").
// OpenEBS's agent on the machine creates each dataset; the ZFSVolume is
// never deleted by the deployer, which would destroy it.

// ZFSVolumes is the namespace of the ZFSVolumes (garrison/openebs).
const ZFSVolumes = "garrison"

// SandboxNamespace is where the agents' sandboxes run; a unit's media share
// has a PersistentVolume and claim there too (MediaClaim).
const SandboxNamespace = "garrison-sandboxes"

// MediaShare is the share of a unit that is its media library, which its
// agents' sandboxes mount.
const MediaShare = "media"

// MediaClaim is the claim of a unit's media share in SandboxNamespace.
func MediaClaim(unit string) string { return "media-" + unit }

// LabelShare names a share's ZFSVolume.
const LabelShare = "garrison.io/share"

// zfsVolume is what the deployer reads of a ZFSVolume.
type zfsVolume struct {
	Metadata struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Spec struct {
		OwnerNodeID string `json:"ownerNodeID"`
	} `json:"spec"`
}

// volumesOf lists the ZFSVolumes with the label selector.
func volumesOf(ctx context.Context, run Runner, selector string) ([]zfsVolume, error) {
	out, err := run(ctx, nil, nil, "kubectl", "get", "zfsvolumes.zfs.openebs.io", "--namespace", ZFSVolumes, "--selector", selector, "--output", "json")
	if err != nil {
		return nil, err
	}
	var l struct {
		Items []zfsVolume `json:"items"`
	}
	if err := json.Unmarshal(out, &l); err != nil {
		return nil, fmt.Errorf("kubectl get zfsvolumes: %w", err)
	}
	return l.Items, nil
}

// EnvNode is the machine that holds an environment's data, from its
// ZFSVolumes, or "" if it has none yet.
func EnvNode(ctx context.Context, run Runner, stackName string) (string, error) {
	vols, err := volumesOf(ctx, run, render.LabelStack+"="+stackName)
	if err != nil || len(vols) == 0 {
		return "", err
	}
	return vols[0].Spec.OwnerNodeID, nil
}

// shareNode is the machine that holds a unit's share, or "".
func shareNode(ctx context.Context, run Runner, unit, share string) (string, error) {
	vols, err := volumesOf(ctx, run, render.LabelUnit+"="+unit+","+LabelShare+"="+share)
	if err != nil || len(vols) == 0 {
		return "", err
	}
	return vols[0].Spec.OwnerNodeID, nil
}

// roomiest is the machine whose pool has the most free space, from
// OpenEBS's ZFSNodes; the main one if none says so.
func roomiest(ctx context.Context, run Runner) (string, error) {
	out, err := run(ctx, nil, nil, "kubectl", "get", "zfsnodes.zfs.openebs.io", "--namespace", ZFSVolumes, "--output", "json")
	if err != nil {
		return "", err
	}
	var l struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Pools []struct {
				Name string `json:"name"`
				Free string `json:"free"`
			} `json:"pools"`
		} `json:"items"`
	}
	if err := json.Unmarshal(out, &l); err != nil {
		return "", fmt.Errorf("kubectl get zfsnodes: %w", err)
	}
	pool := strings.Split(strings.TrimPrefix(render.DefaultDataRoot, "/"), "/")[0]
	best, most := "", int64(-1)
	for _, n := range l.Items {
		for _, p := range n.Pools {
			if p.Name != pool {
				continue
			}
			if free, err := sizeBytes(p.Free); err == nil && free > most {
				best, most = n.Metadata.Name, free
			}
		}
	}
	if best == "" {
		return MainNode(ctx, run)
	}
	return best, nil
}

// PlaceEnv is the machine for an environment's data: where its volumes are,
// or, for one that has none yet, where the shares it mounts are, or else the
// machine with the most free space. "" for a stack without volumes.
func PlaceEnv(ctx context.Context, run Runner, s *stack.Stack, env string) (string, error) {
	shares, datasets := volumeNames(s)
	if len(shares)+len(datasets) == 0 {
		return "", nil
	}
	if n, err := EnvNode(ctx, run, render.StackName(s, env)); err != nil || n != "" {
		return n, err
	}
	for _, sh := range shares {
		if n, err := shareNode(ctx, run, s.Unit, sh); err != nil || n != "" {
			return n, err
		}
	}
	return roomiest(ctx, run)
}

// volumeNames are the shares and the datasets a stack mounts, each once,
// sorted, with each dataset's size.
func volumeNames(s *stack.Stack) (shares []string, datasets map[string]string) {
	datasets = map[string]string{}
	seen := map[string]bool{}
	for _, name := range s.ServiceNames() {
		for _, v := range s.Services[name].Volumes {
			if v.Share != "" {
				if !seen[v.Share] {
					seen[v.Share] = true
					shares = append(shares, v.Share)
				}
			} else {
				datasets[v.Dataset] = v.Size
			}
		}
	}
	sort.Strings(shares)
	return shares, datasets
}

// ensureVolumes makes an environment's volumes and shares on node, the
// machine PlaceEnv chose: their parent datasets, each ZFSVolume, and the
// PersistentVolumes its claims bind (render). An existing dataset from before
// OpenEBS is adopted first (adopt).
func ensureVolumes(ctx context.Context, run Runner, s *stack.Stack, o render.Options, node string) error {
	shares, datasets := volumeNames(s)
	if len(shares)+len(datasets) == 0 {
		return nil
	}
	ctx = WithNode(ctx, node)
	stackName := render.StackName(s, o.Environment)
	ns := render.Namespace(s, o.Environment)
	labels := map[string]string{render.LabelUnit: s.Unit, render.LabelApp: s.Name, render.LabelEnvironment: o.Environment, render.LabelStack: stackName}
	if len(datasets) > 0 {
		parent := render.EnvParent(o.DataRoot, s, o.Environment)
		if err := EnsureDataset(ctx, run, "/"+parent, ""); err != nil {
			return err
		}
		names := make([]string, 0, len(datasets))
		for d := range datasets {
			names = append(names, d)
		}
		sort.Strings(names)
		for _, d := range names {
			size := datasets[d]
			ds := render.VolumeDataset(o.DataRoot, s, o.Environment, d)
			if err := adopt(ctx, run, parent+"/"+d, ds, size); err != nil {
				return err
			}
			vol := render.VolumeName(s, o.Environment, d)
			if err := ensureZFSVolume(ctx, run, vol, parent, node, size, false, labels); err != nil {
				return err
			}
			if err := ensurePV(ctx, run, vol, vol, parent, node, size, ns, render.DataClaim(d), labels); err != nil {
				return err
			}
		}
	}
	for _, sh := range shares {
		if err := EnsureShare(ctx, run, o.DataRoot, s.Unit, sh, node); err != nil {
			return err
		}
		if err := ensurePV(ctx, run, render.SharePV(s.Unit, sh, ns), render.ShareVolume(s.Unit, sh), render.ShareParent(o.DataRoot, s.Unit), node,
			render.ShareSize, ns, render.ShareClaim(sh), labels); err != nil {
			return err
		}
	}
	return nil
}

// EnsureShare makes a unit's share on node, if it has none yet: its dataset
// and its ZFSVolume, shared, so several namespaces mount it. The media share
// also gets its PersistentVolume and claim in the sandboxes' namespace.
func EnsureShare(ctx context.Context, run Runner, root, unit, share, node string) error {
	if root == "" {
		root = render.DefaultDataRoot
	}
	ctx = WithNode(ctx, node)
	parent := render.ShareParent(root, unit)
	if err := EnsureDataset(ctx, run, "/"+parent, ""); err != nil {
		return err
	}
	if err := adopt(ctx, run, parent+"/"+share, render.ShareDataset(root, unit, share), render.ShareSize); err != nil {
		return err
	}
	labels := map[string]string{render.LabelUnit: unit, LabelShare: share}
	vol := render.ShareVolume(unit, share)
	if err := ensureZFSVolume(ctx, run, vol, parent, node, render.ShareSize, true, labels); err != nil {
		return err
	}
	if share != MediaShare {
		return nil
	}
	pv := render.SharePV(unit, share, SandboxNamespace)
	if err := ensurePV(ctx, run, pv, vol, parent, node, render.ShareSize, SandboxNamespace, MediaClaim(unit), labels); err != nil {
		return err
	}
	claim, err := json.Marshal(map[string]any{"apiVersion": "v1", "kind": "PersistentVolumeClaim",
		"metadata": map[string]any{"name": MediaClaim(unit), "namespace": SandboxNamespace, "labels": labels},
		"spec": map[string]any{"accessModes": []string{"ReadWriteOnce"}, "storageClassName": render.StorageClass, "volumeName": pv,
			"resources": map[string]any{"requests": map[string]string{"storage": render.ShareSize}}}})
	if err != nil {
		return err
	}
	_, err = run(ctx, nil, claim, "kubectl", "apply", "--filename", "-")
	return err
}

// adopt renames a dataset made before OpenEBS (old, mounted on the host) to
// the name its ZFSVolume gives it (ds), and makes its mountpoint legacy, so
// OpenEBS mounts it into the pods; its size becomes its quota, which OpenEBS
// sets only on the datasets it creates. Nothing if old does not exist.
func adopt(ctx context.Context, run Runner, old, ds, size string) error {
	if !exists(ctx, run, old) || exists(ctx, run, ds) {
		return nil
	}
	if _, err := run(ctx, nil, nil, "zfs", "rename", old, ds); err != nil {
		return err
	}
	if _, err := run(ctx, nil, nil, "zfs", "set", "mountpoint=legacy", ds); err != nil {
		return err
	}
	_, err := run(ctx, nil, nil, "zfs", "set", "quota="+strings.TrimSuffix(size, "i"), ds)
	return err
}

// ensureZFSVolume creates the ZFSVolume name, of a dataset under parent on
// node, unless it exists: OpenEBS's agent there creates the dataset, with
// size as its quota, or takes the one that is there. An existing one is
// left as it is: its machine does not change.
func ensureZFSVolume(ctx context.Context, run Runner, name, parent, node, size string, shared bool, labels map[string]string) error {
	if _, err := run(ctx, nil, nil, "kubectl", "get", "zfsvolumes.zfs.openebs.io", name, "--namespace", ZFSVolumes, "--output", "name"); err == nil {
		return nil
	}
	n, err := sizeBytes(size)
	if err != nil {
		return err
	}
	spec := map[string]any{"capacity": strconv.FormatInt(n, 10), "fsType": "zfs", "ownerNodeID": node, "poolName": parent, "volumeType": "DATASET"}
	if shared {
		spec["shared"] = "yes"
	}
	obj, err := json.Marshal(map[string]any{"apiVersion": "zfs.openebs.io/v1", "kind": "ZFSVolume",
		"metadata": map[string]any{"name": name, "namespace": ZFSVolumes, "labels": labels}, "spec": spec})
	if err != nil {
		return err
	}
	_, err = run(ctx, nil, obj, "kubectl", "create", "--filename", "-")
	return err
}

// ensurePV applies the PersistentVolume name of the ZFSVolume vol (under
// parent, on node), bound to the claim ns/claim. One released by a claim
// deleted meanwhile is freed for the claim again.
func ensurePV(ctx context.Context, run Runner, name, vol, parent, node, size, ns, claim string, labels map[string]string) error {
	obj, err := json.Marshal(map[string]any{"apiVersion": "v1", "kind": "PersistentVolume",
		"metadata": map[string]any{"name": name, "labels": labels},
		"spec": map[string]any{
			"capacity":                      map[string]string{"storage": size},
			"accessModes":                   []string{"ReadWriteOnce"},
			"persistentVolumeReclaimPolicy": "Retain",
			"storageClassName":              render.StorageClass,
			"claimRef":                      map[string]string{"namespace": ns, "name": claim},
			"csi": map[string]any{"driver": "zfs.csi.openebs.io", "fsType": "zfs", "volumeHandle": vol,
				"volumeAttributes": map[string]string{"openebs.io/poolname": parent}},
			"nodeAffinity": map[string]any{"required": map[string]any{"nodeSelectorTerms": []any{map[string]any{
				"matchExpressions": []any{map[string]any{"key": "openebs.io/nodeid", "operator": "In", "values": []string{node}}}}}}},
		}})
	if err != nil {
		return err
	}
	if _, err := run(ctx, nil, obj, "kubectl", "apply", "--filename", "-"); err != nil {
		return err
	}
	out, err := run(ctx, nil, nil, "kubectl", "get", "persistentvolume", name, "--output", "jsonpath={.status.phase}")
	if err == nil && strings.TrimSpace(string(out)) == "Released" {
		_, err = run(ctx, nil, nil, "kubectl", "patch", "persistentvolume", name, "--type", "merge", "--patch", `{"spec":{"claimRef":{"uid":null,"resourceVersion":null}}}`)
	}
	return err
}

// sizeBytes is a size such as 5Gi (the stack file's) or 7971764Ki (OpenEBS's)
// in bytes.
func sizeBytes(q string) (int64, error) {
	for i, unit := range []string{"Ki", "Mi", "Gi", "Ti", "Pi"} {
		if v, ok := strings.CutSuffix(q, unit); ok {
			n, err := strconv.ParseInt(v, 10, 64)
			return n << (10 * (i + 1)), err
		}
	}
	return strconv.ParseInt(q, 10, 64)
}

// volumeMount is where restores mount a dataset on its machine.
func volumeMount(ds string) string { return path.Join("/run/garrison/restore", strings.ReplaceAll(ds, "/", "_")) }
