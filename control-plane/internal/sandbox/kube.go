package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// Engine runs the sandboxes: Docker, or Kubernetes, where each is a pod.
type Engine interface {
	// Remove removes the sandbox called name, if there is one.
	Remove(ctx context.Context, name string) error
	// CreateVolume makes the sandbox's workspace, kept between sandboxes.
	CreateVolume(ctx context.Context, name string, labels map[string]string) error
	// Create creates the sandbox and returns what names it.
	Create(ctx context.Context, s Spec) (string, error)
	// Pull fetches the image, after Create found none.
	Pull(ctx context.Context, image, auth string) error
	// Attach connects to the sandbox's stdin and stdout; errs gets its stderr.
	Attach(ctx context.Context, id string, errs io.Writer) (io.WriteCloser, io.ReadCloser, error)
	// Start starts it, once attached.
	Start(ctx context.Context, id string) error
	// List lists the sandboxes that carry label.
	List(ctx context.Context, label string) ([]Container, error)
	// Subnets are a network's ranges, where the engine needs the agent
	// service's own address on it (Docker under gVisor); nil otherwise.
	Subnets(ctx context.Context, network string) ([]*net.IPNet, error)
}

// Kubernetes runs each sandbox as a pod in Namespace, with kubectl as the
// agent service's service account, which may do nothing else there. The
// namespace's admission policy (garrison/platform) refuses any pod that is
// not a sandbox: another image, without gVisor, privileges, other volumes.
// The workspace is a PersistentVolumeClaim named like the pod.
type Kubernetes struct {
	Namespace  string
	PullSecret string // for the sandbox image; empty: none
	Kubectl    string // default kubectl
}

func (k *Kubernetes) kubectl() string {
	if k.Kubectl != "" {
		return k.Kubectl
	}
	return "kubectl"
}

func (k *Kubernetes) run(ctx context.Context, stdin []byte, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, k.kubectl(), append([]string{"--namespace", k.Namespace}, args...)...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return out, fmt.Errorf("kubectl %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

var notLabel = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// labelValue makes v a valid label value: other characters become _, at
// most 63, beginning and ending with a letter or digit. The value itself
// is kept as an annotation.
func labelValue(v string) string {
	v = notLabel.ReplaceAllString(v, "_")
	if len(v) > 63 {
		v = v[:63]
	}
	return strings.Trim(v, "_.-")
}

func (k *Kubernetes) meta(name string, labels map[string]string) map[string]any {
	l, a := map[string]string{}, map[string]string{}
	for key, v := range labels {
		l[key] = labelValue(v)
		a[key] = v
	}
	return map[string]any{"name": name, "labels": l, "annotations": a}
}

func (k *Kubernetes) apply(ctx context.Context, obj map[string]any) error {
	data, err := json.Marshal(obj)
	if err != nil {
		return err
	}
	_, err = k.run(ctx, data, "apply", "--filename", "-")
	return err
}

// Remove deletes the pod; its claim stays.
func (k *Kubernetes) Remove(ctx context.Context, name string) error {
	_, err := k.run(ctx, nil, "delete", "pod", name, "--ignore-not-found", "--grace-period", "2", "--wait")
	return err
}

func (k *Kubernetes) CreateVolume(ctx context.Context, name string, labels map[string]string) error {
	return k.apply(ctx, map[string]any{
		"apiVersion": "v1", "kind": "PersistentVolumeClaim", "metadata": k.meta(name, labels),
		"spec": map[string]any{
			"accessModes": []string{"ReadWriteOnce"},
			"resources":   map[string]any{"requests": map[string]string{"storage": "5Gi"}},
		},
	})
}

// Create creates the pod; it starts at once, and Attach waits for it.
func (k *Kubernetes) Create(ctx context.Context, s Spec) (string, error) {
	var env []map[string]string
	for _, kv := range s.Env {
		name, value, _ := strings.Cut(kv, "=")
		env = append(env, map[string]string{"name": name, "value": value})
	}
	mounts := []map[string]any{{"name": "workspace", "mountPath": "/data"}}
	volumes := []map[string]any{{"name": "workspace", "persistentVolumeClaim": map[string]string{"claimName": s.Volume}}}
	if s.Media != "" {
		mounts = append(mounts, map[string]any{"name": "media", "mountPath": MediaTarget})
		// The unit's media share, through its claim here, which the deployer
		// keeps (apply.MediaClaim); the pod then runs on the share's machine.
		volumes = append(volumes, map[string]any{"name": "media", "persistentVolumeClaim": map[string]string{"claimName": "media-" + s.Media}})
	}
	limits := map[string]string{"memory": fmt.Sprint(s.Memory)}
	if s.CPUs > 0 {
		limits["cpu"] = fmt.Sprintf("%dm", int(s.CPUs*1000))
	}
	container := map[string]any{
		"name": "sandbox", "image": s.Image, "env": env,
		// Pi's RPC: its stdin stays open for attaching, again after a detach.
		"stdin": true, "stdinOnce": false, "tty": false,
		"volumeMounts": mounts,
		"resources":    map[string]any{"limits": limits, "requests": map[string]string{"memory": "64Mi", "cpu": "50m"}},
	}
	spec := map[string]any{
		"containers":                   []any{container},
		"volumes":                      volumes,
		"restartPolicy":                "Never",
		"automountServiceAccountToken": false,
		"enableServiceLinks":           false,
		// The pod's pause process is PID 1 and reaps what Pi leaves behind,
		// as Docker's init did.
		"shareProcessNamespace": true,
	}
	if s.Runtime != "" {
		// gVisor: the agent is root inside it, so it installs what a task needs.
		spec["runtimeClassName"] = s.Runtime
		container["securityContext"] = map[string]any{"runAsUser": 0, "runAsGroup": 0}
	} else {
		container["securityContext"] = map[string]any{"capabilities": map[string]any{"drop": []string{"ALL"}}, "allowPrivilegeEscalation": false}
	}
	if k.PullSecret != "" {
		spec["imagePullSecrets"] = []map[string]string{{"name": k.PullSecret}}
	}
	err := k.apply(ctx, map[string]any{"apiVersion": "v1", "kind": "Pod", "metadata": k.meta(s.Name, s.Labels), "spec": spec})
	return s.Name, err
}

// Pull is the kubelet's, with the pull secret.
func (k *Kubernetes) Pull(context.Context, string, string) error { return nil }

// Attach waits until the pod runs, then attaches to its stdin and stdout
// with kubectl attach, which lasts until the returned reader is closed.
func (k *Kubernetes) Attach(ctx context.Context, id string, errs io.Writer) (io.WriteCloser, io.ReadCloser, error) {
	// Long enough for a first pull of the image, which is large, on a
	// machine that does not have it yet (see the DaemonSet sandbox-image).
	wait, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	if _, err := k.run(wait, nil, "wait", "--for=condition=Ready", "pod/"+id, "--timeout=590s"); err != nil {
		logs, _ := k.run(ctx, nil, "logs", "pod/"+id, "--tail", "20")
		events, _ := k.run(ctx, nil, "get", "events", "--field-selector", "involvedObject.name="+id, "--output", "custom-columns=REASON:.reason,MESSAGE:.message", "--no-headers")
		return nil, nil, fmt.Errorf("%w; %s %s", err, strings.TrimSpace(string(events)), strings.TrimSpace(string(logs)))
	}
	// Not with ctx, which is the start's: the attachment outlives it.
	cmd := exec.Command(k.kubectl(), "--namespace", k.Namespace, "attach", "--stdin", "--quiet", "pod/"+id, "--container", "sandbox")
	cmd.Stderr = errs
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, nil, err
	}
	go cmd.Wait()
	return in, attached{ReadCloser: out, cmd: cmd}, nil
}

// attached ends kubectl attach when closed.
type attached struct {
	io.ReadCloser
	cmd *exec.Cmd
}

func (a attached) Close() error {
	if a.cmd.Process != nil {
		a.cmd.Process.Kill()
	}
	return a.ReadCloser.Close()
}

// Start: the pod started when it was created.
func (k *Kubernetes) Start(context.Context, string) error { return nil }

func (k *Kubernetes) List(ctx context.Context, label string) ([]Container, error) {
	out, err := k.run(ctx, nil, "get", "pods", "--selector", label, "--output", "json")
	if err != nil {
		return nil, err
	}
	var l struct {
		Items []struct {
			Metadata struct {
				Name        string            `json:"name"`
				Annotations map[string]string `json:"annotations"`
			} `json:"metadata"`
			Spec struct {
				Containers []struct{ Image string } `json:"containers"`
			} `json:"spec"`
			Status struct{ Phase string } `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(out, &l); err != nil {
		return nil, err
	}
	var res []Container
	for _, p := range l.Items {
		c := Container{ID: p.Metadata.Name, Names: []string{"/" + p.Metadata.Name}, State: strings.ToLower(p.Status.Phase), Labels: p.Metadata.Annotations}
		if len(p.Spec.Containers) > 0 {
			c.Image = p.Spec.Containers[0].Image
		}
		res = append(res, c)
	}
	return res, nil
}

// Subnets: pods reach the agent service by its Service's name.
func (k *Kubernetes) Subnets(context.Context, string) ([]*net.IPNet, error) { return nil, nil }
