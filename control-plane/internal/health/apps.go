package health

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"

	"garrison/control-plane/internal/agents"
	"garrison/control-plane/internal/render"
	"garrison/control-plane/internal/system"
)

// Unlisted is the annotation that keeps an Ingress of the platform off the
// list of apps (value "true"), e.g. the sign-in's own address, which is
// no place to go to.
const Unlisted = "garrison.io/unlisted"

// App is one place people open in a browser: an environment of a unit's app
// (todo, dev-todo, pr-3-todo), or one of the platform's own pages (git,
// chat, grafana). Running and Desired count the pods of its workloads.
type App struct {
	Unit        string   `json:"unit"`
	App         string   `json:"app"`
	Environment string   `json:"environment"`
	Stack       string   `json:"stack"`
	Hosts       []string `json:"hosts"`
	Desired     int      `json:"desired"`
	Running     int      `json:"running"`
}

// Apps lists what runs behind an address, from the Ingress objects, with
// the health of their workloads. A unit's environments are by their labels;
// the platform's by Ingress, as one stack may serve several (control-plane:
// the dashboard). Sorted by unit, then app, then environment.
func (k *Kubernetes) Apps(ctx context.Context) ([]App, error) {
	ingresses, err := k.list(ctx, "ingresses")
	if err != nil {
		return nil, err
	}
	svcs, _, err := k.collect(ctx)
	if err != nil {
		return nil, err
	}
	byKey := map[string]*App{}
	var keys []string
	for _, in := range ingresses {
		l := in.Metadata.Labels
		a := App{Unit: l[render.LabelUnit], App: l[render.LabelApp], Environment: l[render.LabelEnvironment], Stack: l[system.StackLabel]}
		if a.Unit == "" {
			if a.Stack == "" || in.Metadata.Annotations[Unlisted] == "true" {
				continue // Kubernetes's own, or the platform's plumbing
			}
			a.Unit, a.App, a.Environment = agents.PlatformUnit, in.Metadata.Name, "main"
		}
		key := a.Unit + "/" + a.App + "/" + a.Environment
		have := byKey[key]
		if have == nil {
			have = &a
			byKey[key] = have
			keys = append(keys, key)
		}
		for _, r := range in.Spec.Rules {
			if r.Host != "" && !contains(have.Hosts, r.Host) {
				have.Hosts = append(have.Hosts, r.Host)
			}
		}
	}
	var out []App
	for _, key := range keys {
		a := byKey[key]
		if len(a.Hosts) == 0 {
			continue
		}
		for _, s := range svcs {
			if s.Unit != a.Unit || s.Stack != a.Stack {
				continue
			}
			// An app's workloads are those of its environment; the
			// platform's, those of the stack behind its address.
			if a.Unit != agents.PlatformUnit && (s.App != a.App || s.Environment != a.Environment) {
				continue
			}
			a.Desired += s.Desired
			a.Running += s.Running
		}
		sort.Strings(a.Hosts)
		out = append(out, *a)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Unit != out[j].Unit {
			return out[i].Unit < out[j].Unit
		}
		if out[i].App != out[j].App {
			return out[i].App < out[j].App
		}
		return out[i].Environment < out[j].Environment
	})
	return out, nil
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// AppsHandler serves Apps as JSON, for the dashboard, which shows each
// person the ones they may see.
func (k *Kubernetes) AppsHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apps, err := k.Apps(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		if apps == nil {
			apps = []App{}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(apps)
	})
}
