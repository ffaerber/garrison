package home

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
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
	Stack       string   `json:"stack"` // the environment's stack name
	// Snapshot: this person may take a snapshot of it (a person of the unit,
	// or a root admin; not the platform's own).
	Snapshot bool `json:"-"`
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
		view := appView{Unit: a.Unit, App: a.App, Environment: a.Environment, Desired: a.Desired, Running: a.Running, Stack: a.Stack,
			Snapshot: s.Snapshots != nil && a.Unit != agents.PlatformUnit && (v.All || v.Members[a.Unit])}
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
	Units  []appUnit
	Error  string
	Notice string
}

// Snapshots takes a snapshot of an environment on request: the deployer's.
type Snapshots interface {
	Snapshot(ctx context.Context, unit, stack, by string) (string, error)
}

// snapshotPost takes a snapshot of an environment (unit, stack), for a
// person of its unit or a root admin, and goes back to the Apps page.
func (s *Server) snapshotPost(w http.ResponseWriter, r *http.Request, v events.Visibility) {
	if r.Header.Get("Origin") != "https://"+s.Domain {
		http.Error(w, "this form is only taken from the dashboard's own page", http.StatusForbidden)
		return
	}
	unit, stack := r.FormValue("unit"), r.FormValue("stack")
	if unit == agents.PlatformUnit || !(v.All || v.Members[unit]) {
		http.Error(w, "only the people of a unit, and root admins, take its snapshots", http.StatusForbidden)
		return
	}
	name, err := s.Snapshots.Snapshot(r.Context(), unit, stack, v.Person)
	notice := ""
	switch {
	case err != nil:
		log.Printf("home: %s: snapshot of %s/%s: %v", v.Person, unit, stack, err)
		notice = "The snapshot failed: " + err.Error()
	case name == "":
		notice = stack + " holds no data, so there is nothing to snapshot."
	default:
		notice = fmt.Sprintf("Took the snapshot %s of %s. It is kept with the others, and a restore can go back to it.", name, stack)
	}
	http.Redirect(w, r, "/apps?notice="+url.QueryEscape(notice), http.StatusSeeOther)
}

// RemoteSnapshots asks the deployer, with the service key.
func RemoteSnapshots(deployer, key string) Snapshots {
	return remoteSnapshots{url: strings.TrimSuffix(deployer, "/"), key: key}
}

type remoteSnapshots struct{ url, key string }

func (d remoteSnapshots) Snapshot(ctx context.Context, unit, stack, by string) (string, error) {
	body, _ := json.Marshal(map[string]string{"by": by})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.url+"/snapshots/"+url.PathEscape(unit)+"/"+url.PathEscape(stack), bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("X-Garrison-Service", d.key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 3 * time.Minute}).Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(data)))
	}
	var out struct {
		Snapshot string `json:"snapshot"`
	}
	return out.Snapshot, json.Unmarshal(data, &out)
}

type appUnit struct {
	Name string
	Apps []appView
}

func (s *Server) appsPage(w http.ResponseWriter, r *http.Request, v events.Visibility) {
	p := feedPage{Who: v.Person, Domain: s.Domain, All: v.All, Now: s.now(), AppsPage: &appsView{Notice: r.URL.Query().Get("notice")}}
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
