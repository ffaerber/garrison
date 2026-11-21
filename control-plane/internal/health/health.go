// Package health tells Prometheus how the workloads are doing, which the
// kubelet's container metrics cannot: they see only running containers, so
// an app that exits at start hardly shows. Its metrics come from the
// Kubernetes API, per Deployment, StatefulSet and DaemonSet, with the unit,
// app and environment the deployer labels each app with; the platform's
// own workloads, in the namespace garrison, count as the unit garrison.
package health

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"garrison/control-plane/internal/agents"
	"garrison/control-plane/internal/apply"
	"garrison/control-plane/internal/render"
	"garrison/control-plane/internal/system"
)

// Kubernetes reads workloads and pods with kubectl, as the deployer's
// service account.
type Kubernetes struct {
	Run apply.Runner
	Now func() time.Time
}

// Window is how far back failed containers count.
const Window = 10 * time.Minute

// Service is one workload's health. The names are those Swarm's had:
// Desired and Running are pods, Failed the containers that ended in an
// error within Window.
type Service struct {
	Unit, App, Environment, Stack, Service string
	Desired, Running, Failed               int
	Namespace                              string
	Pods                                   []string // their names
}

type object struct {
	Kind     string `json:"kind"`
	Metadata struct {
		Name        string            `json:"name"`
		Namespace   string            `json:"namespace"`
		Labels      map[string]string `json:"labels"`
		Annotations map[string]string `json:"annotations"`
	} `json:"metadata"`
	Spec struct {
		Replicas *int `json:"replicas"`
		Selector struct {
			MatchLabels map[string]string `json:"matchLabels"`
		} `json:"selector"`
	} `json:"spec"`
	Status struct {
		DesiredNumberScheduled int `json:"desiredNumberScheduled"`
		// Of a pod:
		Phase             string `json:"phase"`
		ContainerStatuses []struct {
			Ready     bool `json:"ready"`
			LastState struct {
				Terminated *struct {
					ExitCode   int       `json:"exitCode"`
					FinishedAt time.Time `json:"finishedAt"`
				} `json:"terminated"`
			} `json:"lastState"`
			State struct {
				Terminated *struct {
					ExitCode   int       `json:"exitCode"`
					FinishedAt time.Time `json:"finishedAt"`
				} `json:"terminated"`
			} `json:"state"`
		} `json:"containerStatuses"`
	} `json:"status"`
}

func (k *Kubernetes) list(ctx context.Context, kinds string) ([]object, error) {
	out, err := k.Run(ctx, nil, nil, "kubectl", "get", kinds, "--all-namespaces", "--output", "json")
	if err != nil {
		return nil, err
	}
	var l struct {
		Items []object `json:"items"`
	}
	if err := json.Unmarshal(out, &l); err != nil {
		return nil, fmt.Errorf("kubectl get %s: %w", kinds, err)
	}
	return l.Items, nil
}

// Sandbox is an agent's sandbox, a pod of its own (garrison.sandbox), and
// whom it works for: its annotations, which keep what labels cannot hold.
type Sandbox struct {
	Namespace, Pod, Unit, Agent, Person, Chat string
}

// Services reads every workload's health.
func (k *Kubernetes) Services(ctx context.Context) ([]Service, error) {
	svcs, _, err := k.collect(ctx)
	return svcs, err
}

// collect reads every workload's health, and the sandboxes.
func (k *Kubernetes) collect(ctx context.Context) ([]Service, []Sandbox, error) {
	workloads, err := k.list(ctx, "deployments,statefulsets,daemonsets")
	if err != nil {
		return nil, nil, err
	}
	pods, err := k.list(ctx, "pods")
	if err != nil {
		return nil, nil, err
	}
	var sandboxes []Sandbox
	for _, p := range pods {
		if p.Metadata.Labels["garrison.sandbox"] != "true" || p.Status.Phase != "Running" {
			continue
		}
		a := p.Metadata.Annotations
		sandboxes = append(sandboxes, Sandbox{Namespace: p.Metadata.Namespace, Pod: p.Metadata.Name,
			Unit: a["garrison.unit"], Agent: a["garrison.agent"], Person: a["garrison.person"], Chat: a["garrison.chat"]})
	}
	now := time.Now()
	if k.Now != nil {
		now = k.Now()
	}
	var res []Service
	for _, w := range workloads {
		l := w.Metadata.Labels
		stack := l[system.StackLabel]
		if stack == "" && l[render.LabelUnit] == "" {
			continue // Kubernetes's own, such as CoreDNS
		}
		h := Service{Unit: l[render.LabelUnit], App: l[render.LabelApp], Environment: l[render.LabelEnvironment], Stack: stack,
			Service: w.Metadata.Name, Namespace: w.Metadata.Namespace}
		if h.Unit == "" {
			// The platform's own stacks: postgres, gitea, control-plane ...
			h.Unit, h.App, h.Environment = agents.PlatformUnit, stack, "main"
		}
		h.Desired = 1
		if w.Spec.Replicas != nil {
			h.Desired = *w.Spec.Replicas
		}
		if w.Kind == "DaemonSet" {
			h.Desired = w.Status.DesiredNumberScheduled
		}
		for _, p := range pods {
			if p.Metadata.Namespace != w.Metadata.Namespace || !selects(w.Spec.Selector.MatchLabels, p.Metadata.Labels) {
				continue
			}
			h.Pods = append(h.Pods, p.Metadata.Name)
			ready := p.Status.Phase == "Running" && len(p.Status.ContainerStatuses) > 0
			for _, c := range p.Status.ContainerStatuses {
				ready = ready && c.Ready
				for _, t := range []*struct {
					ExitCode   int       `json:"exitCode"`
					FinishedAt time.Time `json:"finishedAt"`
				}{c.LastState.Terminated, c.State.Terminated} {
					if t != nil && t.ExitCode != 0 && now.Sub(t.FinishedAt) < Window {
						h.Failed++
					}
				}
			}
			if ready {
				h.Running++
			}
		}
		res = append(res, h)
	}
	sort.Slice(res, func(i, j int) bool { return res[i].Service < res[j].Service })
	return res, sandboxes, nil
}

// selects tells whether a selector's labels are all among labels.
func selects(selector, labels map[string]string) bool {
	if len(selector) == 0 {
		return false
	}
	for k, v := range selector {
		if labels[k] != v {
			return false
		}
	}
	return true
}

// Handler serves the metrics in Prometheus's text format.
func (k *Kubernetes) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		svcs, sandboxes, err := k.collect(r.Context())
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		var b strings.Builder
		up := 1
		if err != nil {
			up = 0
			fmt.Fprintf(&b, "# %s\n", strings.ReplaceAll(err.Error(), "\n", " "))
		}
		b.WriteString("# HELP garrison_cluster_up Whether the Kubernetes API answered.\n# TYPE garrison_cluster_up gauge\n")
		fmt.Fprintf(&b, "garrison_cluster_up %d\n", up)
		for _, m := range []struct {
			name, help string
			v          func(Service) int
		}{
			{"garrison_service_replicas_desired", "Pods the workload wants.", func(s Service) int { return s.Desired }},
			{"garrison_service_replicas_running", "Pods that are running and ready.", func(s Service) int { return s.Running }},
			{"garrison_service_failed_tasks", fmt.Sprintf("Containers that ended in an error in the last %s.", Window), func(s Service) int { return s.Failed }},
		} {
			fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s gauge\n", m.name, m.help, m.name)
			for _, s := range svcs {
				fmt.Fprintf(&b, "%s{unit=%q,app=%q,environment=%q,stack=%q,service=%q} %d\n",
					m.name, s.Unit, s.App, s.Environment, s.Stack, s.Service, m.v(s))
			}
		}
		// The kubelet's container metrics name only namespace and pod; joined
		// with this (* on (namespace, pod) group_left (stack, service) ...),
		// they get the unit, stack and service a pod belongs to.
		b.WriteString("# HELP garrison_pod_info A workload's pod, by namespace and name; always 1.\n# TYPE garrison_pod_info gauge\n")
		for _, s := range svcs {
			for _, p := range s.Pods {
				fmt.Fprintf(&b, "garrison_pod_info{namespace=%q,pod=%q,unit=%q,app=%q,environment=%q,stack=%q,service=%q} 1\n",
					s.Namespace, p, s.Unit, s.App, s.Environment, s.Stack, s.Service)
			}
		}
		// Joined the same way, a sandbox's metrics name its agent and person.
		b.WriteString("# HELP garrison_sandbox_info An agent's sandbox, by namespace and pod; always 1.\n# TYPE garrison_sandbox_info gauge\n")
		for _, s := range sandboxes {
			fmt.Fprintf(&b, "garrison_sandbox_info{namespace=%q,pod=%q,unit=%q,agent=%q,person=%q,chat=%q} 1\n",
				s.Namespace, s.Pod, s.Unit, s.Agent, s.Person, s.Chat)
		}
		io.WriteString(w, b.String())
	})
}
