package gitops

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"garrison/control-plane/internal/apply"
	"garrison/control-plane/internal/events"
	"garrison/control-plane/internal/stack"
)

// moves carries out the move requests of a unit's config, moves/*.yaml: an
// environment whose data is on another machine than the request names is
// moved there (apply.MoveEnv) and deployed again, at the commit it runs,
// there or, if the move failed early, where it was. A request is tried once
// per commit until the deployer restarts: a failed one would stop the app on
// every sync.
func (c *Config) moves(ctx context.Context, ns, cfgDir string, stacks map[string]*stack.Stack, deployed map[string]App) []error {
	files, _ := filepath.Glob(filepath.Join(cfgDir, stack.MoveDir, "*.yaml"))
	var errs []error
	for _, file := range files {
		rel := stack.MoveDir + "/" + filepath.Base(file)
		at := fmt.Sprintf("%s/config/%s", ns, rel)
		data, err := os.ReadFile(file)
		var m *stack.Move
		if err == nil {
			m, err = stack.ParseMove(data)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", at, err))
			continue
		}
		s, ok := stacks[m.Stack]
		if !ok {
			errs = append(errs, fmt.Errorf("%s: there is no stack %s that could be read", at, m.Stack))
			continue
		}
		app, ok := deployed[appKey(ns, m.Stack, m.Environment)]
		if !ok {
			errs = append(errs, fmt.Errorf("%s: %s is not deployed in %s", at, m.Stack, m.Environment))
			continue
		}
		node, err := apply.EnvNode(ctx, c.Run, app.Stack)
		if err != nil {
			log.Printf("apps: %s: where %s is: %v", at, app.Stack, err)
			continue
		}
		if node == m.To || node == "" {
			continue // there already, or nothing to move
		}
		id, err := c.git(ctx, "-C", cfgDir, "log", "-1", "--format=%H", "--", rel)
		if err != nil || id == "" {
			errs = append(errs, fmt.Errorf("%s: its commit: %v", at, err))
			continue
		}
		if c.moveTried == nil {
			c.moveTried = map[string]bool{}
		}
		if c.moveTried[app.Stack+"@"+id] {
			continue
		}
		c.moveTried[app.Stack+"@"+id] = true
		log.Printf("apps: moving %s from %s to %s, as %s asks", app.Stack, node, m.To, at)
		ev := events.Event{Kind: events.KindMove, Status: events.StatusRunning, Unit: ns, Commit: id,
			Title: fmt.Sprintf("moving %s with its data from %s to %s, as %s asks", app.Stack, node, m.To, rel), Key: "move:" + app.Stack + "@" + id}
		c.record(ctx, ev)
		pipe := c.Pipe
		if pipe == nil {
			pipe = apply.NodePipe(c.Run)
		}
		started := time.Now()
		moved, err := apply.MoveEnv(ctx, c.Run, pipe, "", s, m.Environment, m.To, apply.SnapshotStamp(started))
		c.snapsChanged = true
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", at, err))
			ev.Status, ev.Detail = events.StatusFailed, err.Error()
			ev.Title = fmt.Sprintf("%s was not moved to %s", app.Stack, m.To)
			if moved {
				ev.Title = fmt.Sprintf("%s moved to %s, with a problem", app.Stack, m.To)
			}
		}
		o, aerr := c.options(ctx, s, m.Environment, app.Commit, cfgDir)
		if aerr == nil {
			aerr = apply.Stack(ctx, c.Run, s, o)
		}
		if aerr != nil {
			errs = append(errs, fmt.Errorf("%s: %s not started again: %w", at, app.Stack, aerr))
			if err == nil {
				ev.Status, ev.Detail = events.StatusFailed, aerr.Error()
				ev.Title = fmt.Sprintf("%s moved to %s, but did not start there", app.Stack, m.To)
			}
		} else if err == nil {
			ev.Status = events.StatusOK
			ev.Title = fmt.Sprintf("moved %s with its data from %s to %s, down %s", app.Stack, node, m.To, time.Since(started).Round(time.Second))
		}
		c.record(ctx, ev)
	}
	return errs
}
