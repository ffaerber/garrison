package home

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"garrison/control-plane/internal/agents"
	"garrison/control-plane/internal/events"
	"garrison/control-plane/internal/health"
)

// Apps says what runs behind which address: health.Kubernetes.Apps, read
// from the deployer.
type Apps interface {
	Apps(ctx context.Context) ([]health.App, error)
}

// RemoteApps reads the apps from the deployer at base (e.g.
// http://deployer:8090), at most every 10 seconds: the page and the API are
// asked often, and the list changes only with a deploy.
func RemoteApps(base string) Apps {
	return &remoteApps{url: strings.TrimSuffix(base, "/") + "/apps", client: &http.Client{Timeout: 15 * time.Second}}
}

type remoteApps struct {
	url    string
	client *http.Client

	mu   sync.Mutex
	at   time.Time
	apps []health.App
}

func (r *remoteApps) Apps(ctx context.Context) ([]health.App, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.at.IsZero() && time.Since(r.at) < 10*time.Second {
		return r.apps, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("the deployer answered %s", resp.Status)
	}
	var apps []health.App
	if err := json.NewDecoder(resp.Body).Decode(&apps); err != nil {
		return nil, fmt.Errorf("the deployer's apps: %w", err)
	}
	r.apps, r.at = apps, time.Now()
	return apps, nil
}

// SeesApp tells whether the person may see a unit's app on the list: the
// platform's are everyone's; a unit's are for those who may read its config,
// which is everyone signed in for a public unit and its people for a
// private one.
func SeesApp(v events.Visibility, unit string) bool {
	return v.All || unit == agents.PlatformUnit || v.Members[unit] || v.Repos[unit+"/config"]
}

// appView is one app on the page and in the API: its addresses as links.
type appView struct {
	Unit        string   `json:"unit"`
	App         string   `json:"app"`
	Environment string   `json:"environment"`
	URLs        []string `json:"urls"`
	Desired     int      `json:"desired"`
	Running     int      `json:"running"`
	State       string   `json:"state"` // up, starting or down
}

// visibleApps are the apps this person may see, with their state.
func (s *Server) visibleApps(ctx context.Context, v events.Visibility) ([]appView, error) {
	apps, err := s.Apps.Apps(ctx)
	if err != nil {
		return nil, err
	}
	out := []appView{}
	for _, a := range apps {
		if !SeesApp(v, a.Unit) {
			continue
		}
		view := appView{Unit: a.Unit, App: a.App, Environment: a.Environment, Desired: a.Desired, Running: a.Running}
		for _, h := range a.Hosts {
			view.URLs = append(view.URLs, "https://"+h+"/")
		}
		switch {
		case a.Desired > 0 && a.Running >= a.Desired:
			view.State = "up"
		case a.Running > 0:
			view.State = "starting"
		default:
			view.State = "down"
		}
		out = append(out, view)
	}
	return out, nil
}

// appsView is the Apps page: the apps by unit, garrison's first.
type appsView struct {
	Units []appUnit
	Error string
}

type appUnit struct {
	Name string
	Apps []appView
}

func (s *Server) appsPage(w http.ResponseWriter, r *http.Request, v events.Visibility) {
	p := feedPage{Who: v.Person, Domain: s.Domain, All: v.All, Now: s.now(), AppsPage: &appsView{}}
	apps, err := s.visibleApps(r.Context(), v)
	if err != nil {
		log.Printf("home: apps: %v", err)
		p.AppsPage.Error = "The list of apps cannot be read right now: " + err.Error()
	}
	for _, a := range apps {
		n := len(p.AppsPage.Units)
		if n == 0 || p.AppsPage.Units[n-1].Name != a.Unit {
			p.AppsPage.Units = append(p.AppsPage.Units, appUnit{Name: a.Unit})
			n++
		}
		p.AppsPage.Units[n-1].Apps = append(p.AppsPage.Units[n-1].Apps, a)
	}
	// garrison's own first, then the units by name, as the deployer sorts them.
	for i, u := range p.AppsPage.Units {
		if u.Name == agents.PlatformUnit && i > 0 {
			p.AppsPage.Units = append([]appUnit{u}, append(p.AppsPage.Units[:i:i], p.AppsPage.Units[i+1:]...)...)
			break
		}
	}
	s.render(w, "page", p)
}

func (s *Server) apiApps(w http.ResponseWriter, r *http.Request, v events.Visibility) {
	apps, err := s.visibleApps(r.Context(), v)
	if err != nil {
		log.Printf("home: api apps: %v", err)
		http.Error(w, "the list of apps cannot be read right now", http.StatusServiceUnavailable)
		return
	}
	writeJSON(w, apps)
}
