// Package home serves the dashboard at the installation's domain: for now,
// the feed of everything that happens in the home cloud and how it was
// handled. Traefik puts it behind the sign-in, which names the person in
// a header; each person sees only what they may read.
package home

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"garrison/control-plane/internal/events"
)

// UserHeader names the person; the sign-in in front sets it, replacing
// whatever the browser sent.
const UserHeader = "X-Auth-Request-Preferred-Username"

// Access says what a person may see.
type Access interface {
	Visibility(ctx context.Context, person string) (events.Visibility, error)
}

// Server serves the dashboard.
type Server struct {
	Events events.Store
	Access Access
	// Secrets, if set, backs the secrets page (secrets.Store).
	Secrets Secrets
	Domain  string
	Now     func() time.Time

	mu    sync.Mutex
	cache map[string]cached
}

type cached struct {
	v  events.Visibility
	at time.Time
}

//go:embed page.html
var files embed.FS

var page = template.Must(template.New("page.html").Funcs(template.FuncMap{
	"ago":    ago,
	"stamp":  func(t time.Time) string { return t.Local().Format("2006-01-02 15:04:05") },
	"iso":    func(t time.Time) string { return t.UTC().Format(time.RFC3339) },
	"short":  func(s string) string { return s[:min(7, len(s))] },
	"group":  group,
	"source": sourceName,
	"entry":  func(e events.Event, now time.Time, this bool) entry { return entry{E: e, Now: now, This: this} },
}).ParseFS(files, "page.html"))

// entry is one line of a feed: the event, the time to tell its age by, and
// whether it is the one a page is about.
type entry struct {
	E    events.Event
	Now  time.Time
	This bool
}

func (s *Server) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Handler serves the pages.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /{$}", s.signedIn(s.feed))
	mux.Handle("GET /feed", s.signedIn(s.more))
	mux.Handle("GET /e/{id}", s.signedIn(s.event))
	mux.Handle("GET /api/events", s.signedIn(s.apiList))
	mux.Handle("GET /api/events/{id}", s.signedIn(s.apiEvent))
	mux.Handle("GET /secrets", s.signedIn(s.secretsPage))
	mux.Handle("POST /secrets/{ns}", s.signedIn(s.secretsPost))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	return securityHeaders(mux)
}

func securityHeaders(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline'; frame-ancestors 'none'")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "same-origin")
		h.ServeHTTP(w, r)
	})
}

type handler func(w http.ResponseWriter, r *http.Request, v events.Visibility)

func (s *Server) signedIn(h handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		who := r.Header.Get(UserHeader)
		if who == "" {
			http.Error(w, "sign in first", http.StatusUnauthorized)
			return
		}
		v, err := s.visibility(r.Context(), who)
		if err != nil {
			log.Printf("home: %s: %v", who, err)
			http.Error(w, "cannot tell what you may see right now; try again", http.StatusServiceUnavailable)
			return
		}
		h(w, r, v)
	})
}

// visibility asks Access, at most every 20 seconds per person.
func (s *Server) visibility(ctx context.Context, who string) (events.Visibility, error) {
	s.mu.Lock()
	c, ok := s.cache[who]
	s.mu.Unlock()
	if ok && s.now().Sub(c.at) < 20*time.Second {
		return c.v, nil
	}
	v, err := s.Access.Visibility(ctx, who)
	if err != nil {
		return v, err
	}
	s.mu.Lock()
	if s.cache == nil {
		s.cache = map[string]cached{}
	}
	s.cache[who] = cached{v: v, at: s.now()}
	s.mu.Unlock()
	return v, nil
}

// What the feed can be narrowed to.
var views = []struct{ Name, Label string }{
	{"", "Everything"},
	{"changes", "Code and reviews"},
	{"deploys", "Deploys and alerts"},
	{"agents", "Agents"},
	{"problems", "Problems"},
}

func query(r *http.Request, v events.Visibility) (events.Query, url.Values) {
	f := url.Values{}
	q := events.Query{Visibility: v, Limit: 60}
	if ns := r.URL.Query().Get("ns"); ns != "" {
		q.Namespace = ns
		f.Set("ns", ns)
	}
	switch view := r.URL.Query().Get("view"); view {
	case "changes":
		q.Kinds = []string{events.KindPush, events.KindBranchCreated, events.KindBranchDeleted, events.KindRepoCreated, events.KindRepoDeleted,
			events.KindPROpened, events.KindPRUpdated, events.KindPRMerged, events.KindPRClosed, events.KindPRReopened, events.KindApproved, events.KindRejected, events.KindApprovalCheck}
		f.Set("view", view)
	case "deploys":
		q.Kinds = []string{events.KindPipeline, events.KindDeploy, events.KindUndeploy, events.KindRestore, events.KindAlert}
		f.Set("view", view)
	case "agents":
		q.Sources = []string{events.SourceGateway, events.SourceAgents}
		f.Set("view", view)
	case "problems":
		q.Problems = true
		f.Set("view", view)
	}
	if t := strings.TrimSpace(r.URL.Query().Get("q")); t != "" {
		q.Text = t
		f.Set("q", t)
	}
	q.After, _ = strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	q.Before, _ = strconv.ParseInt(r.URL.Query().Get("before"), 10, 64)
	return q, f
}

type feedPage struct {
	Who        string
	Domain     string
	All        bool
	Events     []events.Event
	Waiting    []events.Event // questions agents asked this person, still open
	Namespaces []string
	Views      []struct{ Name, Label string }
	Filter     url.Values
	Query      string // the filter, encoded, for the live updates
	More       int64  // the next page starts below this ID
	Now        time.Time
	Fragment   bool // only the entries, for the live updates and the next page
	Event      *events.Event
	Thread     []events.Event
	Error      string
	Secrets    *secretsView // the secrets page
	HasSecrets bool         // the dashboard has a secrets page
}

func (s *Server) render(w http.ResponseWriter, name string, p feedPage) {
	p.HasSecrets = s.Secrets != nil
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := page.ExecuteTemplate(w, name, p); err != nil {
		log.Printf("home: render %s: %v", name, err)
	}
}

func (s *Server) feed(w http.ResponseWriter, r *http.Request, v events.Visibility) {
	q, f := query(r, v)
	p := feedPage{Who: v.Person, Domain: s.Domain, All: v.All, Views: views, Filter: f, Query: f.Encode(), Now: s.now()}
	es, err := s.Events.List(r.Context(), q)
	if err != nil {
		log.Printf("home: feed: %v", err)
		p.Error = "The feed cannot be read right now: " + err.Error()
	}
	p.Events = es
	if len(es) == q.Limit {
		p.More = es[len(es)-1].ID
	}
	p.Namespaces, _ = s.Events.Namespaces(r.Context(), v)
	// What agents asked this person and wait for, above everything else.
	p.Waiting, _ = s.Events.List(r.Context(), events.Query{Visibility: v, Kinds: []string{events.KindQuestion},
		Actor: v.Person, Status: events.StatusWaiting, Limit: 20})
	s.render(w, "page", p)
}

// more answers the live updates (after=ID) and the next page (before=ID)
// with the entries alone.
func (s *Server) more(w http.ResponseWriter, r *http.Request, v events.Visibility) {
	q, f := query(r, v)
	es, err := s.Events.List(r.Context(), q)
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	p := feedPage{Events: es, Filter: f, Query: f.Encode(), Now: s.now(), Fragment: true}
	if q.Before > 0 && len(es) == q.Limit {
		p.More = es[len(es)-1].ID
	}
	s.render(w, "entries", p)
}

func (s *Server) event(w http.ResponseWriter, r *http.Request, v events.Visibility) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	e, ok, err := s.Events.Get(r.Context(), id, v)
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	if !ok {
		http.NotFound(w, r)
		return
	}
	thread, err := s.Events.Related(r.Context(), e, v)
	if err != nil {
		log.Printf("home: thread of %d: %v", id, err)
	}
	s.render(w, "page", feedPage{Who: v.Person, Domain: s.Domain, All: v.All, Event: &e, Thread: thread, Now: s.now(), Views: views})
}

// apiList answers the feed's filters (ns, view, q, after, before, limit) as JSON.
func (s *Server) apiList(w http.ResponseWriter, r *http.Request, v events.Visibility) {
	q, _ := query(r, v)
	q.Limit, _ = strconv.Atoi(r.URL.Query().Get("limit"))
	es, err := s.Events.List(r.Context(), q)
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	if es == nil {
		es = []events.Event{}
	}
	writeJSON(w, es)
}

// apiEvent answers one event and its thread as JSON.
func (s *Server) apiEvent(w http.ResponseWriter, r *http.Request, v events.Visibility) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	e, ok, err := s.Events.Get(r.Context(), id, v)
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	if !ok {
		http.NotFound(w, r)
		return
	}
	thread, err := s.Events.Related(r.Context(), e, v)
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	if thread == nil {
		thread = []events.Event{}
	}
	writeJSON(w, map[string]any{"event": e, "thread": thread})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func ago(now, t time.Time) string {
	d := now.Sub(t)
	switch {
	case d < 45*time.Second:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%d min ago", int(d.Minutes()+0.5))
	case d < 24*time.Hour:
		return fmt.Sprintf("%d h ago", int(d.Hours()))
	case d < 7*24*time.Hour:
		return t.Local().Format("Mon 15:04")
	}
	return t.Local().Format("2 Jan 15:04")
}

// group is the colour family of a kind.
func group(e events.Event) string {
	switch e.Kind {
	case events.KindPush, events.KindBranchCreated, events.KindBranchDeleted, events.KindRepoCreated, events.KindRepoDeleted:
		return "code"
	case events.KindPROpened, events.KindPRUpdated, events.KindPRMerged, events.KindPRClosed, events.KindPRReopened,
		events.KindApproved, events.KindRejected, events.KindApprovalCheck:
		return "review"
	case events.KindPipeline, events.KindDeploy, events.KindUndeploy, events.KindRestore, events.KindAlert:
		return "deploy"
	}
	return "agent"
}

func sourceName(s string) string {
	switch s {
	case events.SourceGitea:
		return "Gitea"
	case events.SourceDeployer:
		return "deployer"
	case events.SourceGateway:
		return "gateway"
	case events.SourceAgents:
		return "agent service"
	case events.SourceAlerts:
		return "Alertmanager"
	case events.SourceHome:
		return "dashboard"
	}
	return s
}
