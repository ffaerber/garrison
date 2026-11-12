// Package apply deploys a rendered stack to the local Swarm: it creates the
// ZFS datasets and Swarm secrets the stack needs, then runs the deploy.
package apply

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"homecloud/control-plane/internal/render"
	"homecloud/control-plane/internal/stack"
)

// Runner runs a command with optional extra environment (KEY=VALUE, on top
// of this process's) and stdin, and returns its stdout.
type Runner func(ctx context.Context, env []string, stdin []byte, name string, args ...string) ([]byte, error)

// Exec is the Runner that runs real commands.
func Exec(ctx context.Context, env []string, stdin []byte, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	if env != nil {
		cmd.Env = append(os.Environ(), env...)
	}
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
				err = EnsureDataset(ctx, run, render.SharePath(o.DataRoot, s.Namespace, v.Share), "")
			} else {
				err = EnsureDataset(ctx, run, render.DatasetPath(o.DataRoot, s, o.Environment, v.Dataset), v.Size)
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

	// --with-registry-auth: images from the installation's registry need the
	// credentials the deployer signed in with.
	_, err = run(ctx, nil, compose, "docker", "stack", "deploy", "--detach=true", "--resolve-image", "never", "--with-registry-auth", "--compose-file", "-", render.StackName(s, o.Environment))
	return err
}

// EnsureDataset creates the ZFS dataset mounted at path if it is missing and,
// if size is set, sets its quota. The dataset name is the path without the
// leading slash.
func EnsureDataset(ctx context.Context, run Runner, path, size string) error {
	dataset := strings.TrimPrefix(path, "/")
	if _, err := run(ctx, nil, nil, "zfs", "list", "-H", "-o", "name", dataset); err != nil {
		if _, err := run(ctx, nil, nil, "zfs", "create", "-p", dataset); err != nil {
			return err
		}
	}
	if size == "" {
		return nil
	}
	// ZFS spells 5Gi as 5G; both mean powers of 1024.
	_, err := run(ctx, nil, nil, "zfs", "set", "quota="+strings.TrimSuffix(size, "i"), dataset)
	return err
}

// ensureSecret creates a Swarm secret with a random value if it is missing.
// The value never leaves this function.
func ensureSecret(ctx context.Context, run Runner, name string) error {
	if _, err := run(ctx, nil, nil, "docker", "secret", "inspect", "--format", "{{.ID}}", name); err == nil {
		return nil
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return err
	}
	_, err := run(ctx, nil, []byte(hex.EncodeToString(buf)), "docker", "secret", "create", name, "-")
	return err
}

// HostExec returns a Runner for a deployer running in a container: ZFS
// commands run on the host, in a short-lived privileged container of image
// that enters the host's namespaces. Everything else runs directly.
func HostExec(image string) Runner {
	return func(ctx context.Context, env []string, stdin []byte, name string, args ...string) ([]byte, error) {
		if name == "zfs" {
			args = append([]string{"run", "--rm", "--privileged", "--pid=host", "--network=none",
				"--entrypoint", "nsenter", image, "-t", "1", "-m", "-u", "-i", "-n", "--", "zfs"}, args...)
			name = "docker"
		}
		return Exec(ctx, env, stdin, name, args...)
	}
}
