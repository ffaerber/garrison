package gitops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"garrison/control-plane/internal/apply"
	"garrison/control-plane/internal/events"
	"garrison/control-plane/internal/render"
	"garrison/control-plane/internal/stack"
)

// SnapshotsEvery is how often the deployer lists the snapshots again when it
// took none itself: Sanoid takes them on its own schedule.
const SnapshotsEvery = 5 * time.Minute

// EnvSnapshots are the snapshots of one environment's datasets, kept in
// WorkDir/snapshots.json by its Swarm stack name (the hostname) for the
// gateway.
type EnvSnapshots struct {
	Namespace string       `json:"namespace"`
	Snapshots []apply.Snap `json:"snapshots"` // oldest first
}

// ReadSnapshots reads what the deployer last listed.
func ReadSnapshots(workDir string) (map[string]EnvSnapshots, time.Time, error) {
	file := filepath.Join(workDir, "snapshots.json")
	data, err := os.ReadFile(file)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]EnvSnapshots{}, time.Time{}, nil
	}
	if err != nil {
		return nil, time.Time{}, err
	}
	var out map[string]EnvSnapshots
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, time.Time{}, err
	}
	st, _ := os.Stat(file)
	return out, st.ModTime(), nil
}

// listSnapshots writes snapshots.json when the deployer changed snapshots,
// and otherwise every SnapshotsEvery.
func (c *Config) listSnapshots(ctx context.Context) error {
	if !c.snapsChanged && time.Since(c.snapsListed) < SnapshotsEvery {
		return nil
	}
	root := strings.TrimPrefix(render.DefaultDataRoot, "/")
	if _, err := c.Run(ctx, nil, nil, "zfs", "list", "-H", "-o", "name", root); err != nil {
		return nil // no app has data yet
	}
	// <root>/<namespace>/stacks/<hostname> are three below root; their
	// snapshots one more.
	all, err := apply.ListSnapshots(ctx, c.Run, root, 4)
	if err != nil {
		return err
	}
	out := map[string]EnvSnapshots{}
	for ds, snaps := range all {
		parts := strings.Split(strings.TrimPrefix(ds, root+"/"), "/")
		if len(parts) != 3 || parts[1] != "stacks" {
			continue
		}
		out[parts[2]] = EnvSnapshots{Namespace: parts[0], Snapshots: snaps}
	}
	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	file := filepath.Join(c.WorkDir, "snapshots.json")
	if err := os.WriteFile(file+".tmp", append(data, '\n'), 0o644); err != nil {
		return err
	}
	if err := os.Rename(file+".tmp", file); err != nil {
		return err
	}
	c.snapsChanged, c.snapsListed = false, time.Now()
	return nil
}

// restores carries out the restore requests of a namespace's config,
// restores/*.yaml, each once: the commit that last changed the file is
// recorded on the environment's dataset. The environment is deployed again
// afterwards, at the commit it runs, whether the restore worked or not.
func (c *Config) restores(ctx context.Context, ns, cfgDir string, stacks map[string]*stack.Stack, deployed map[string]App) []error {
	files, _ := filepath.Glob(filepath.Join(cfgDir, stack.RestoreDir, "*.yaml"))
	var errs []error
	for _, file := range files {
		rel := stack.RestoreDir + "/" + filepath.Base(file)
		at := fmt.Sprintf("%s/config/%s", ns, rel)
		data, err := os.ReadFile(file)
		var r *stack.Restore
		if err == nil {
			r, err = stack.ParseRestore(data)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", at, err))
			continue
		}
		id, err := c.git(ctx, "-C", cfgDir, "log", "-1", "--format=%H", "--", rel)
		if err != nil || id == "" {
			errs = append(errs, fmt.Errorf("%s: its commit: %v", at, err))
			continue
		}
		s, ok := stacks[r.Stack]
		if !ok {
			errs = append(errs, fmt.Errorf("%s: there is no stack %s that could be read", at, r.Stack))
			continue
		}
		if apply.Restored(ctx, c.Run, "", s, r.Environment) == id {
			continue // done before
		}
		app, ok := deployed[appKey(ns, r.Stack, r.Environment)]
		if !ok {
			errs = append(errs, fmt.Errorf("%s: %s is not deployed in %s", at, r.Stack, r.Environment))
			continue
		}
		// A request that failed is not tried again until the deployer
		// restarts: it would stop the app on every sync.
		if c.restoreTried == nil {
			c.restoreTried = map[string]bool{}
		}
		if c.restoreTried[app.Stack+"@"+id] {
			continue
		}
		c.restoreTried[app.Stack+"@"+id] = true
		log.Printf("apps: restoring %s to %s, as %s asks", app.Stack, r.Snapshot, at)
		restore := events.Event{Kind: events.KindRestore, Status: events.StatusRunning, Namespace: ns, Commit: id,
			Title: fmt.Sprintf("restoring %s to the snapshot %s, as %s asks", app.Stack, r.Snapshot, rel), Key: "restore:" + app.Stack + "@" + id}
		c.record(ctx, restore)
		err = apply.RestoreEnv(ctx, c.Run, "", s, r.Environment, r.Snapshot, apply.SnapshotStamp(time.Now()), id)
		c.snapsChanged = true
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: not restored: %w", at, err))
			restore.Status, restore.Detail = events.StatusFailed, err.Error()
			restore.Title = fmt.Sprintf("%s was not restored to %s", app.Stack, r.Snapshot)
		} else {
			log.Printf("apps: restored %s to %s", app.Stack, r.Snapshot)
			restore.Status, restore.Title = events.StatusOK, fmt.Sprintf("restored %s to the snapshot %s", app.Stack, r.Snapshot)
		}
		c.record(ctx, restore)
		if err := apply.Stack(ctx, c.Run, s, c.options(s, r.Environment, app.Commit)); err != nil {
			errs = append(errs, fmt.Errorf("%s: %s not started again: %w", at, app.Stack, err))
		}
	}
	return errs
}
