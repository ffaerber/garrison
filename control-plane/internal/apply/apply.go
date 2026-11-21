// Package apply deploys a rendered stack to Kubernetes: it creates the ZFS
// datasets, the namespace and the secrets the stack needs, then applies it.
package apply

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
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

// Stack deploys one environment of s, in its own namespace. It is safe to
// run again: existing datasets and secrets are kept, and kubectl apply
// changes only what differs, and prunes what the stack no longer has.
func Stack(ctx context.Context, run Runner, s *stack.Stack, o render.Options) error {
	if o.DataRoot == "" {
		o.DataRoot = render.DefaultDataRoot
	}
	manifests, err := render.Kubernetes(s, o)
	if err != nil {
		return err
	}
	ns := render.Namespace(s, o.Environment)

	// Its data: on the machine that has it, or the one chosen for it.
	node, err := PlaceEnv(ctx, run, s, o.Environment)
	if err != nil {
		return err
	}
	if err := ensureVolumes(ctx, run, s, o, node); err != nil {
		return err
	}
	// The namespace first, then what its pods need before they start: the
	// pull secret and the secrets; then the rest.
	namespace, err := json.Marshal(render.NamespaceObject(s, o.Environment))
	if err != nil {
		return err
	}
	if _, err := run(ctx, nil, namespace, "kubectl", "apply", "--filename", "-"); err != nil {
		return err
	}
	if err := copyPullSecret(ctx, run, ns); err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, name := range s.ServiceNames() {
		for _, sec := range s.Services[name].Secrets {
			if seen[sec] {
				continue
			}
			seen[sec] = true
			if st, ok := o.Secrets[sec]; ok {
				err = ensureStored(ctx, run, ns, sec, st)
			} else {
				err = ensureSecret(ctx, run, ns, sec)
			}
			if err != nil {
				return err
			}
		}
	}
	args := []string{"apply", "--namespace", ns, "--filename", "-", "--prune", "--selector", render.LabelStack + "=" + render.StackName(s, o.Environment)}
	for _, k := range Pruned {
		args = append(args, "--prune-allowlist", k)
	}
	_, err = run(ctx, nil, manifests, "kubectl", args...)
	return err
}

// Pruned are the kinds an app's objects are of, which kubectl apply
// deletes when the stack no longer has one of them.
var Pruned = []string{
	"apps/v1/Deployment", "core/v1/Service", "networking.k8s.io/v1/Ingress",
	"networking.k8s.io/v1/NetworkPolicy", "traefik.io/v1alpha1/Middleware",
}

// Remove takes an environment off the cluster: everything the deployer
// applied in its namespace (Pruned). Its datasets are kept, and so are its
// Secrets and the namespace that holds them: data kept with a generated
// password needs that password when the app comes back.
func Remove(ctx context.Context, run Runner, stackName string) error {
	_, err := run(ctx, nil, nil, "kubectl", "delete", "deployments,services,ingresses,networkpolicies,middlewares.traefik.io",
		"--namespace", render.NamespaceOf(stackName), "--all", "--ignore-not-found", "--wait=false")
	return err
}

// copyPullSecret puts the platform's pull secret (render.PullSecret, in
// garrison) into ns, for the installation's images.
func copyPullSecret(ctx context.Context, run Runner, ns string) error {
	out, err := run(ctx, nil, nil, "kubectl", "get", "secret", render.PullSecret, "--namespace", "garrison", "--output", "json")
	if err != nil {
		return err
	}
	var sec map[string]any
	if err := json.Unmarshal(out, &sec); err != nil {
		return err
	}
	sec["metadata"] = map[string]any{"name": render.PullSecret, "namespace": ns}
	data, err := json.Marshal(sec)
	if err != nil {
		return err
	}
	_, err = run(ctx, nil, data, "kubectl", "apply", "--filename", "-")
	return err
}

// secretObject is a Secret of one key, both named name, as a pod mounts it
// at /run/secrets/<name>. The value goes to kubectl on stdin.
func secretObject(ns, name, value string, labels, annotations map[string]string) ([]byte, error) {
	return json.Marshal(map[string]any{
		"apiVersion": "v1", "kind": "Secret",
		"metadata":   map[string]any{"name": name, "namespace": ns, "labels": labels, "annotations": annotations},
		"stringData": map[string]string{name: value},
	})
}

// ensureStored puts a stored secret's value into its Secret, unless that
// holds this version already. A new version restarts the pods, whose
// template carries the version.
func ensureStored(ctx context.Context, run Runner, ns, name string, st render.Stored) error {
	out, err := run(ctx, nil, nil, "kubectl", "get", "secret", name, "--namespace", ns, "--ignore-not-found",
		"--output", `jsonpath={.metadata.annotations.garrison\.io/version}`)
	if err == nil && strings.TrimSpace(string(out)) == st.Version {
		return nil
	}
	data, err := secretObject(ns, name, st.Value, map[string]string{render.LabelStored: "true"}, map[string]string{"garrison.io/version": st.Version})
	if err != nil {
		return err
	}
	_, err = run(ctx, nil, data, "kubectl", "apply", "--filename", "-")
	return err
}

// ensureSecret makes a Secret with a random value if it is missing, or if
// it held a stored secret that is gone. The value never leaves this
// function but to kubectl, on stdin.
func ensureSecret(ctx context.Context, run Runner, ns, name string) error {
	out, err := run(ctx, nil, nil, "kubectl", "get", "secret", name, "--namespace", ns, "--ignore-not-found",
		"--output", `jsonpath={.metadata.name} {.metadata.labels.garrison\.stored}`)
	if err != nil {
		return err
	}
	switch strings.TrimSpace(string(out)) {
	case name:
		return nil // generated earlier
	case name + " true":
		// Was a stored secret: it gets a value of its own again.
		if _, err := run(ctx, nil, nil, "kubectl", "delete", "secret", name, "--namespace", ns); err != nil {
			return err
		}
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return err
	}
	data, err := secretObject(ns, name, hex.EncodeToString(buf), nil, nil)
	if err != nil {
		return err
	}
	_, err = run(ctx, nil, data, "kubectl", "create", "--filename", "-")
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
