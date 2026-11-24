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

	"garrison/control-plane/internal/events"
	"garrison/control-plane/internal/gateway"
	"garrison/control-plane/internal/sandbox"
)

// Running says which agents work now: sandbox.Manager.Turns, read from
// the agent service.
type Running interface {
	Running(ctx context.Context) ([]sandbox.Running, error)
}

// RemoteRunning reads the agents working now from the agent service at base
// (e.g. http://agents:8070) with the service key, at most every 3 seconds:
// every page asks, for the count in its header.
func RemoteRunning(base, serviceKey string) Running {
	return &remoteRunning{url: strings.TrimSuffix(base, "/") + "/internal/running", key: serviceKey,
		client: &http.Client{Timeout: 5 * time.Second}}
}

type remoteRunning struct {
	url, key string
	client   *http.Client

	mu   sync.Mutex
	at   time.Time
	list []sandbox.Running
}

func (r *remoteRunning) Running(ctx context.Context) ([]sandbox.Running, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.at.IsZero() && time.Since(r.at) < 3*time.Second {
		return r.list, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set(gateway.ServiceHeader, r.key)
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("the agent service answered %s", resp.Status)
	}
	var list []sandbox.Running
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return nil, fmt.Errorf("the agent service's running agents: %w", err)
	}
	r.list, r.at = list, time.Now()
	return list, nil
}

// SeesRun tells whether the person may see what an agent does now, as in
// the feed: the unit's people, the person it works for, and root admins.
// Everyone else only counts it.
func SeesRun(v events.Visibility, r sandbox.Running) bool {
	return v.All || v.Members[r.Unit] || (v.Person != "" && r.For == v.Person)
}

// runView is one agent at work, as the person may see it.
type runView struct {
	Unit  string    `json:"unit"`
	Agent string    `json:"agent"`
	For   string    `json:"for"`
	How   string    `json:"how"` // in a chat, a routine's name, or which agent asked
	Chain []string  `json:"chain"`
	Since time.Time `json:"since"`
}

// runningView is what the dashboard says of the agents at work.
type runningView struct {
	Total  int       `json:"total"`  // all of them, in every unit
	Units  int       `json:"units"`  // the units they work in
	Shown  []runView `json:"agents"` // those this person may see
	Hidden int       `json:"hidden"` // the rest
	Error  string    `json:"error,omitempty"`
	Now    time.Time `json:"-"`
}

// how says in words what started a run.
func how(startedBy string) string {
	switch {
	case startedBy == "person" || startedBy == "":
		return "in a chat"
	case strings.HasPrefix(startedBy, "routine "):
		parts := strings.Split(strings.TrimPrefix(startedBy, "routine "), "/")
		return "routine " + parts[len(parts)-1]
	case strings.HasPrefix(startedBy, "agent "):
		return "asked by " + strings.TrimPrefix(startedBy, "agent ")
	}
	return startedBy
}

// runningFor is what this person may see of the agents at work; nil
// without Running.
func (s *Server) runningFor(ctx context.Context, v events.Visibility) *runningView {
	if s.Running == nil {
		return nil
	}
	view := &runningView{Shown: []runView{}, Now: s.now()}
	list, err := s.Running.Running(ctx)
	if err != nil {
		log.Printf("home: running agents: %v", err)
		view.Error = "Which agents work now cannot be read right now."
		return view
	}
	units := map[string]bool{}
	for _, r := range list {
		view.Total++
		units[r.Unit] = true
		if !SeesRun(v, r) {
			view.Hidden++
			continue
		}
		view.Shown = append(view.Shown, runView{Unit: r.Unit, Agent: r.Agent, For: r.For, How: how(r.StartedBy), Chain: r.Chain, Since: r.Since})
	}
	view.Units = len(units)
	return view
}

// runningBox is the section of the feed, alone, for its live updates.
func (s *Server) runningBox(w http.ResponseWriter, r *http.Request, v events.Visibility) {
	w.Header().Set("Cache-Control", "no-store")
	s.render(w, "running", feedPage{Now: s.now(), Fragment: true, Running: s.runningFor(r.Context(), v)})
}

func (s *Server) apiRunning(w http.ResponseWriter, r *http.Request, v events.Visibility) {
	view := s.runningFor(r.Context(), v)
	if view.Error != "" {
		http.Error(w, view.Error, http.StatusServiceUnavailable)
		return
	}
	writeJSON(w, view)
}
