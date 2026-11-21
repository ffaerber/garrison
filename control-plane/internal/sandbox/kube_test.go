package sandbox

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeKubectl writes a kubectl that keeps its arguments and stdin in dir.
func fakeKubectl(t *testing.T) (string, string) {
	dir := t.TempDir()
	script := "#!/bin/sh\necho \"$@\" >> " + dir + "/args\ncat > " + dir + "/stdin\n"
	path := filepath.Join(dir, "kubectl")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path, dir
}

func TestKubernetesPod(t *testing.T) {
	kubectl, dir := fakeKubectl(t)
	k := &Kubernetes{Namespace: "garrison-sandboxes", PullSecret: "registry-pull", Kubectl: kubectl}
	labels := map[string]string{Label: "true", "garrison.agent": "todo/developer", "garrison.chat": "channel:abc", UnitLabel: "todo"}
	id, err := k.Create(context.Background(), Spec{Name: "garrison-agent-1", Image: "git.example.com/garrison/sandbox:1", Labels: labels,
		Volume: "garrison-agent-1", Media: "todo", Memory: 2 << 30, CPUs: 1.5, Runtime: "gvisor",
		Env: []string{"GARRISON_API=http://agents.garrison.svc.cluster.local:8071", "GARRISON_PROMPT=a=b"}})
	if err != nil || id != "garrison-agent-1" {
		t.Fatal(id, err)
	}
	args, _ := os.ReadFile(filepath.Join(dir, "args"))
	if !strings.HasPrefix(string(args), "--namespace garrison-sandboxes apply --filename -") {
		t.Errorf("kubectl %s", args)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "stdin"))
	var pod struct {
		Metadata struct {
			Labels, Annotations map[string]string
		}
		Spec struct {
			RuntimeClassName      string
			ShareProcessNamespace bool
			RestartPolicy         string
			ImagePullSecrets      []struct{ Name string }
			Containers            []struct {
				Stdin     bool
				Env       []struct{ Name, Value string }
				Resources struct{ Limits map[string]string }
				SecurityContext struct{ RunAsUser *int }
			}
			Volumes []struct {
				PersistentVolumeClaim *struct{ ClaimName string }
				HostPath              *struct{ Path string }
			}
		}
	}
	if err := json.Unmarshal(data, &pod); err != nil {
		t.Fatalf("%v: %s", err, data)
	}
	c := pod.Spec.Containers[0]
	switch {
	case pod.Metadata.Labels["garrison.agent"] != "todo_developer" || pod.Metadata.Annotations["garrison.agent"] != "todo/developer":
		t.Errorf("labels %v, annotations %v", pod.Metadata.Labels, pod.Metadata.Annotations)
	case pod.Metadata.Labels["garrison.chat"] != "channel_abc":
		t.Errorf("chat label %q", pod.Metadata.Labels["garrison.chat"])
	case pod.Spec.RuntimeClassName != "gvisor" || !pod.Spec.ShareProcessNamespace || pod.Spec.RestartPolicy != "Never":
		t.Errorf("spec %+v", pod.Spec)
	case len(pod.Spec.ImagePullSecrets) != 1 || pod.Spec.ImagePullSecrets[0].Name != "registry-pull":
		t.Errorf("pull secrets %+v", pod.Spec.ImagePullSecrets)
	case !c.Stdin || c.SecurityContext.RunAsUser == nil || *c.SecurityContext.RunAsUser != 0:
		t.Errorf("container %+v", c)
	case c.Resources.Limits["memory"] != "2147483648" || c.Resources.Limits["cpu"] != "1500m":
		t.Errorf("limits %v", c.Resources.Limits)
	case c.Env[1].Name != "GARRISON_PROMPT" || c.Env[1].Value != "a=b":
		t.Errorf("env %+v", c.Env)
	case pod.Spec.Volumes[0].PersistentVolumeClaim.ClaimName != "garrison-agent-1" || pod.Spec.Volumes[1].PersistentVolumeClaim.ClaimName != "media-todo":
		t.Errorf("volumes %+v", pod.Spec.Volumes)
	}
}

func TestLabelValue(t *testing.T) {
	for in, want := range map[string]string{"todo/developer": "todo_developer", "alice": "alice", "-x:": "x", strings.Repeat("a", 70): strings.Repeat("a", 63)} {
		if got := labelValue(in); got != want {
			t.Errorf("labelValue(%q) = %q, want %q", in, got, want)
		}
	}
}
