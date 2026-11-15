// Package health tells Prometheus how the Swarm services are doing, which
// cAdvisor cannot: it sees only running containers, so an app that exits at
// start hardly shows. Its metrics come from Docker's API, per service, with
// the namespace, app and environment the deployer labels each app with;
// the platform's own services count as the namespace system.
package health

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strings"
	"time"

	"garrison/control-plane/internal/render"
)

// Docker reads services and tasks from Docker's API.
type Docker struct {
	Socket string // e.g. /var/run/docker.sock
	HTTP   *http.Client
	Now    func() time.Time
}

func (d *Docker) client() *http.Client {
	if d.HTTP != nil {
		return d.HTTP
	}
	return &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", d.Socket)
		},
	}}
}

func (d *Docker) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://docker"+path, nil)
	if err != nil {
		return err
	}
	resp, err := d.client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 500))
		return fmt.Errorf("docker %s: %s: %s", path, resp.Status, b)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

type service struct {
	ID   string `json:"ID"`
	Spec struct {
		Name   string            `json:"Name"`
		Labels map[string]string `json:"Labels"`
	} `json:"Spec"`
}

type task struct {
	ServiceID    string `json:"ServiceID"`
	DesiredState string `json:"DesiredState"`
	Status       struct {
		State     string    `json:"State"`
		Timestamp time.Time `json:"Timestamp"`
		Err       string    `json:"Err"`
	} `json:"Status"`
}

// Window is how far back failed tasks count.
const Window = 10 * time.Minute

// Service is one Swarm service's health.
type Service struct {
	Namespace, App, Environment, Stack, Service string
	Desired, Running, Failed                    int
}

// Services reads every service's health.
func (d *Docker) Services(ctx context.Context) ([]Service, error) {
	var svcs []service
	if err := d.get(ctx, "/services", &svcs); err != nil {
		return nil, err
	}
	var tasks []task
	if err := d.get(ctx, "/tasks", &tasks); err != nil {
		return nil, err
	}
	now := time.Now()
	if d.Now != nil {
		now = d.Now()
	}
	byID := map[string]*Service{}
	var out []*Service
	for _, s := range svcs {
		l := s.Spec.Labels
		stack := l["com.docker.stack.namespace"]
		h := &Service{Namespace: l[render.LabelNamespace], App: l[render.LabelApp], Environment: l[render.LabelEnvironment], Stack: stack, Service: s.Spec.Name}
		if h.Namespace == "" {
			// The platform's own stacks: postgres, gitea, control-plane ...
			h.Namespace, h.App, h.Environment = "system", stack, "main"
			if stack == "" {
				h.App = s.Spec.Name
			}
		}
		byID[s.ID] = h
		out = append(out, h)
	}
	for _, t := range tasks {
		h, ok := byID[t.ServiceID]
		if !ok {
			continue
		}
		if t.DesiredState == "running" {
			h.Desired++
			if t.Status.State == "running" {
				h.Running++
			}
		}
		if (t.Status.State == "failed" || t.Status.State == "rejected") && now.Sub(t.Status.Timestamp) < Window {
			h.Failed++
		}
	}
	res := make([]Service, 0, len(out))
	for _, h := range out {
		res = append(res, *h)
	}
	sort.Slice(res, func(i, j int) bool { return res[i].Service < res[j].Service })
	return res, nil
}

// Handler serves the metrics in Prometheus's text format.
func (d *Docker) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		svcs, err := d.Services(r.Context())
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		var b strings.Builder
		up := 1
		if err != nil {
			up = 0
			fmt.Fprintf(&b, "# %s\n", strings.ReplaceAll(err.Error(), "\n", " "))
		}
		b.WriteString("# HELP garrison_docker_up Whether Docker's API answered.\n# TYPE garrison_docker_up gauge\n")
		fmt.Fprintf(&b, "garrison_docker_up %d\n", up)
		for _, m := range []struct {
			name, help string
			v          func(Service) int
		}{
			{"garrison_service_replicas_desired", "Tasks Swarm wants running.", func(s Service) int { return s.Desired }},
			{"garrison_service_replicas_running", "Tasks that are running.", func(s Service) int { return s.Running }},
			{"garrison_service_failed_tasks", fmt.Sprintf("Tasks that failed in the last %s.", Window), func(s Service) int { return s.Failed }},
		} {
			fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s gauge\n", m.name, m.help, m.name)
			for _, s := range svcs {
				fmt.Fprintf(&b, "%s{namespace=%q,app=%q,environment=%q,stack=%q,service=%q} %d\n",
					m.name, s.Namespace, s.App, s.Environment, s.Stack, s.Service, m.v(s))
			}
		}
		io.WriteString(w, b.String())
	})
}
