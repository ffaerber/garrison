// Package apply deploys a rendered stack to the local Swarm: it creates the
// ZFS datasets and Swarm secrets the stack needs, then runs the deploy.
package apply

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os/exec"
	"strings"

	"homecloud/controller/internal/render"
	"homecloud/controller/internal/stack"
)

// Runner runs a command with optional stdin and returns its stdout.
type Runner func(ctx context.Context, stdin []byte, name string, args ...string) ([]byte, error)

// Exec is the Runner that runs real commands.
func Exec(ctx context.Context, stdin []byte, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return out, fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// Stack deploys one environment of s. It is safe to run again: existing
// datasets and secrets are kept, and Swarm updates the stack in place.
func Stack(ctx context.Context, run Runner, s *stack.Stack, o render.Options) error {
	if o.DataRoot == "" {
		o.DataRoot = render.DefaultDataRoot
	}
	compose, err := render.Swarm(s, o)
	if err != nil {
		return err
	}

	for _, name := range s.ServiceNames() {
		svc := s.Services[name]
		for _, v := range svc.Volumes {
			if v.Share != "" {
				err = ensureDataset(ctx, run, render.SharePath(o.DataRoot, s.Namespace, v.Share), "")
			} else {
				err = ensureDataset(ctx, run, render.DatasetPath(o.DataRoot, s, o.Environment, v.Dataset), v.Size)
			}
			if err != nil {
				return err
			}
		}
		for _, sec := range svc.Secrets {
			if err := ensureSecret(ctx, run, render.SecretName(s, o.Environment, sec)); err != nil {
				return err
			}
		}
	}

	_, err = run(ctx, compose, "docker", "stack", "deploy", "--detach=true", "--compose-file", "-", render.StackName(s, o.Environment))
	return err
}

// ensureDataset creates the ZFS dataset mounted at path if it is missing and
// sets its quota. The dataset name is the path without the leading slash.
func ensureDataset(ctx context.Context, run Runner, path, size string) error {
	dataset := strings.TrimPrefix(path, "/")
	if _, err := run(ctx, nil, "zfs", "list", "-H", "-o", "name", dataset); err != nil {
		if _, err := run(ctx, nil, "zfs", "create", "-p", dataset); err != nil {
			return err
		}
	}
	if size == "" {
		return nil
	}
	// ZFS spells 5Gi as 5G; both mean powers of 1024.
	_, err := run(ctx, nil, "zfs", "set", "quota="+strings.TrimSuffix(size, "i"), dataset)
	return err
}

// ensureSecret creates a Swarm secret with a random value if it is missing.
// The value never leaves this function.
func ensureSecret(ctx context.Context, run Runner, name string) error {
	if _, err := run(ctx, nil, "docker", "secret", "inspect", "--format", "{{.ID}}", name); err == nil {
		return nil
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return err
	}
	_, err := run(ctx, []byte(hex.EncodeToString(buf)), "docker", "secret", "create", name, "-")
	return err
}
