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

	"garrison/control-plane/internal/render"
	"garrison/control-plane/internal/stack"
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
				err = EnsureDataset(ctx, run, render.SharePath(o.DataRoot, s.Unit, v.Share), "")
			} else {
				err = EnsureDataset(ctx, run, render.DatasetPath(o.DataRoot, s, o.Environment, v.Dataset), v.Size)
			}
			if err != nil {
				return err
			}
		}
		for _, sec := range svc.Secrets {
			name := o.SwarmSecret(s, sec)
			if st, ok := o.Secrets[sec]; ok {
				err = ensureStored(ctx, run, name, st.Value)
			} else {
				err = ensureSecret(ctx, run, name)
			}
			if err != nil {
				return err
			}
		}
	}

	// --with-registry-auth: images from the installation's registry need the
	// credentials the deployer signed in with.
	_, err = run(ctx, nil, compose, "docker", "stack", "deploy", "--detach=true", "--resolve-image", "never", "--with-registry-auth", "--compose-file", "-", render.StackName(s, o.Environment))
	if err != nil {
		return err
	}
	pruneStored(ctx, run, s, o)
	return nil
}

// ensureStored creates the Swarm secret holding a version of a stored
// secret, if it is missing. The value goes to Docker on stdin.
func ensureStored(ctx context.Context, run Runner, name, value string) error {
	if _, err := run(ctx, nil, nil, "docker", "secret", "inspect", "--format", "{{.ID}}", name); err == nil {
		return nil
	}
	_, err := run(ctx, nil, []byte(value), "docker", "secret", "create", "--label", "garrison.stored=true", name, "-")
	return err
}

// pruneStored removes the earlier versions of an environment's stored
// secrets. Swarm refuses while a task still uses one, as during the update
// just started: those go on a later deploy.
func pruneStored(ctx context.Context, run Runner, s *stack.Stack, o render.Options) {
	seen := map[string]bool{}
	for _, name := range s.ServiceNames() {
		for _, sec := range s.Services[name].Secrets {
			if seen[sec] {
				continue
			}
			seen[sec] = true
			prefix := render.SecretName(s, o.Environment, sec) + "."
			out, err := run(ctx, nil, nil, "docker", "secret", "ls", "--filter", "name="+prefix, "--filter", "label=garrison.stored", "--format", "{{.Name}}")
			if err != nil {
				continue
			}
			for _, have := range strings.Fields(string(out)) {
				if strings.HasPrefix(have, prefix) && have != o.SwarmSecret(s, sec) {
					run(ctx, nil, nil, "docker", "secret", "rm", have)
				}
			}
		}
	}
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

// HostExec returns a Runner for a deployer running in a pod with the host's
// process namespace (hostPID) and privileges: ZFS and rsync (which copies
// datasets back from snapshots) run on the host, entering the namespaces of
// its first process. Everything else runs directly in the pod.
func HostExec() Runner {
	return func(ctx context.Context, env []string, stdin []byte, name string, args ...string) ([]byte, error) {
		if name == "zfs" || name == "rsync" {
			args = append([]string{"-t", "1", "-m", "-u", "-i", "-n", "--", name}, args...)
			name = "nsenter"
		}
		return Exec(ctx, env, stdin, name, args...)
	}
}
