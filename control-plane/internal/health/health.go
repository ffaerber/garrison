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
	"sync"
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

	mu       sync.Mutex
	restarts map[string]*restarts // by namespace/pod/container
}

// restarts is what is known of one container's failures. Kubernetes
// restarts a failing container in the same pod and keeps only its last
// exit, so the failures are counted as its restart count grows.
type restarts struct {
	count  int         // its restart count, when last seen
	failed []time.Time // its failures within Window
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
		// Of an Ingress:
		Rules []struct {
			HTTP struct {
				Paths []struct {
					Backend struct {
						Service struct {
							Name string `json:"name"`
							Port struct {
								Name   string `json:"name"`
								Number int    `json:"number"`
							} `json:"port"`
						} `json:"service"`
					} `json:"backend"`
				} `json:"paths"`
			} `json:"http"`
		} `json:"rules"`
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
			Name         string `json:"name"`
			Ready        bool   `json:"ready"`
			RestartCount int    `json:"restartCount"`
			LastState    struct {
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

// Route is how Traefik names the service of an app's Ingress in its
// metrics (<namespace>-<service>-<port>@kubernetes), and whose app it is.
type Route struct {
	Traefik, Unit, App, Environment, Stack string
}

// Services reads every workload's health.
func (k *Kubernetes) Services(ctx context.Context) ([]Service, error) {
	svcs, _, err := k.collect(ctx)
	return svcs, err
}

// routes lists the apps' routes, from their Ingress objects.
func (k *Kubernetes) routes(ctx context.Context) ([]Route, error) {
	ingresses, err := k.list(ctx, "ingresses")
	if err != nil {
		return nil, err
	}
	var out []Route
	for _, in := range ingresses {
		l := in.Metadata.Labels
		if l[render.LabelUnit] == "" {
			continue // the platform's own
		}
		for _, r := range in.Spec.Rules {
			for _, p := range r.HTTP.Paths {
				b := p.Backend.Service
				port := b.Port.Name
				if port == "" {
					port = fmt.Sprint(b.Port.Number)
				}
				out = append(out, Route{Traefik: in.Metadata.Namespace + "-" + b.Name + "-" + port + "@kubernetes",
					Unit: l[render.LabelUnit], App: l[render.LabelApp], Environment: l[render.LabelEnvironment], Stack: l[system.StackLabel]})
			}
		}
	}
	return out, nil
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
	seen := map[string]bool{}
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
		} else {
			// An app's service is <stack>_<service>, as the gateway's tools,
			// the alerts and the feed name it: each environment is a
			// namespace, so the name alone says nothing of whose it is.
			h.Service = stack + "_" + w.Metadata.Name
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
				h.Failed += k.failures(p.Metadata.Namespace+"/"+p.Metadata.Name+"/"+c.Name, c.RestartCount,
					c.LastState.Terminated, c.State.Terminated, now, seen)
			}
			if ready {
				h.Running++
			}
		}
		res = append(res, h)
	}
	k.forget(seen)
	sort.Slice(res, func(i, j int) bool { return res[i].Service < res[j].Service })
	return res, sandboxes, nil
}

// terminated is how a container last ended.
type terminated = struct {
	ExitCode   int       `json:"exitCode"`
	FinishedAt time.Time `json:"finishedAt"`
}

// failures counts the container key's failures within Window, from what
// it shows now (restartCount, its last and current termination) and what
// was seen of it before. The first time, only the terminations it shows
// count; after that, each restart after an error counts, at the time it is
// seen, so none is lost between two looks.
func (k *Kubernetes) failures(key string, count int, last, current *terminated, now time.Time, seen map[string]bool) int {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.restarts == nil {
		k.restarts = map[string]*restarts{}
	}
	seen[key] = true
	r, known := k.restarts[key]
	if !known {
		r = &restarts{count: count}
		k.restarts[key] = r
		for _, t := range []*terminated{last, current} {
			if t != nil && t.ExitCode != 0 {
				r.failed = append(r.failed, t.FinishedAt)
			}
		}
	} else {
		if count > r.count && last != nil && last.ExitCode != 0 {
			for i := r.count; i < count; i++ {
				r.failed = append(r.failed, now)
			}
		}
		r.count = count
	}
	kept := r.failed[:0]
	for _, t := range r.failed {
		if now.Sub(t) < Window {
			kept = append(kept, t)
		}
	}
	r.failed = kept
	return len(kept)
}

// forget drops the containers not seen this time: their pods are gone.
func (k *Kubernetes) forget(seen map[string]bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	for key := range k.restarts {
		if !seen[key] {
			delete(k.restarts, key)
		}
	}
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
		var routes []Route
		if err == nil {
			routes, err = k.routes(r.Context())
		}
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
		// Traefik's metrics name a route's service as Traefik does; joined
		// with this (* on (service) group_left (stack, unit, app, environment)),
		// they name its app. The alert HighErrorRate does so.
		b.WriteString("# HELP garrison_route_info An app's route, as Traefik names its service; always 1.\n# TYPE garrison_route_info gauge\n")
		for _, r := range routes {
			fmt.Fprintf(&b, "garrison_route_info{service=%q,unit=%q,app=%q,environment=%q,stack=%q} 1\n", r.Traefik, r.Unit, r.App, r.Environment, r.Stack)
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
