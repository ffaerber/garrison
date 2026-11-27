// Package home serves the dashboard at the installation's domain: for now,
// the feed of everything that happens in Garrison and how it was
// handled. Traefik puts it behind the sign-in, which names the person in
// a header; each person sees only what they may read.
package home

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"garrison/control-plane/internal/changes"
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
	// Providers, if set with Secrets, backs the Models page, where root
	// admins add LLM providers and set their keys.
	Providers Providers
	// Changes, if set, backs the cards of pull requests.
	Changes Changes
	// Apps, if set, backs the Apps page and /api/apps: every address of
	// Garrison and its units' apps, as each person may see them.
	Apps Apps
	// Running, if set, says which agents work now: a count in every page's
	// header, who and what on the feed, and /api/running.
	Running Running
	// Traces, if set, backs the Agents page: each agent's turns, step by
	// step, for those who may read their answers.
	Traces Traces
	// MergeWait is how long Approve and merge waits for the checks the
	// approval starts (garrison/approval); 0: 40 seconds.
	MergeWait time.Duration
	Domain    string
	Now       func() time.Time
	// Setup, if set, backs the setup at /setup, open with the token in
	// SetupTokenFile until a root admin and the providers are there.
	Setup          Setup
	SetupTokenFile string
	// PlatformUser is the platform's own account, which is no person.
	PlatformUser string
	// HTTP asks the LLM providers the setup tests; nil: a client of its own.
	HTTP *http.Client
	// People and Invites, if set, back the People page and /join: links
	// that make people or set their passwords, sent by hand.
	People  People
	Invites events.Invites
	// Units, if set, backs the Units page: each unit's unit.md.
	Units UnitPages
	// Snapshots, if set, takes snapshots from the Apps page.
	Snapshots Snapshots
	// Shots, if set, serves the screenshots agents showed in a chat.
	Shots events.Shots

	mu    sync.Mutex
	cache map[string]cached
	draft setupDraft
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
	"icon":   icon,
	"svg":    svgIcon,
	"sprite": func() template.HTML { return sprite },
	"title":  titleHTML,
	"agent":  agentTag,
	"person": personTag,
	"source": sourceName,
	"entry":  func(e events.Event, now time.Time, this bool) entry { return entry{E: e, Now: now, This: this} },
	"card":   func(c changes.Card) string { return changes.Path(c.Unit, c.Repo, c.Number) },
	"join":   func(s []string) string { return strings.Join(s, ", ") },
	"chain":  func(s []string) string { return strings.Join(s, " → ") },
	"where":  where,
	"took":   took,
	"name":   func(id string) string { _, n, _ := strings.Cut(id, "/"); return n },
	"unitOf": func(id string) string { u, _, _ := strings.Cut(id, "/"); return u },
	"state": func(s string) string {
		if s == "" {
			return "pending"
		}
		return s
	},
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
	mux.HandleFunc("GET /setup", s.setupPage)
	mux.HandleFunc("POST /setup", s.setupPost)
	mux.HandleFunc("GET /join", s.joinPage)
	mux.HandleFunc("POST /join", s.joinPost)
	if s.Shots != nil {
		mux.Handle("GET /shots/{file}", s.signedIn(s.shotFile))
	}
	if s.Units != nil {
		mux.Handle("GET /u", s.signedIn(s.unitsPage))
		mux.Handle("GET /u/{unit}", s.signedIn(s.unitsPage))
	}
	if s.People != nil && s.Invites != nil {
		mux.Handle("GET /people", s.signedIn(s.peoplePage))
		mux.Handle("POST /people", s.signedIn(s.peoplePost))
	}
	mux.Handle("GET /{$}", s.signedIn(s.feed))
	mux.Handle("GET /feed", s.signedIn(s.more))
	mux.Handle("GET /waiting", s.signedIn(s.waiting))
	mux.Handle("GET /e/{id}", s.signedIn(s.event))
	mux.Handle("GET /api/events", s.signedIn(s.apiList))
	mux.Handle("GET /api/events/{id}", s.signedIn(s.apiEvent))
	if s.Running != nil {
		mux.Handle("GET /running", s.signedIn(s.runningBox))
		mux.Handle("GET /api/running", s.signedIn(s.apiRunning))
	}
	if s.Apps != nil {
		mux.Handle("GET /apps", s.signedIn(s.appsPage))
		mux.Handle("GET /api/apps", s.signedIn(s.apiApps))
		if s.Snapshots != nil {
			mux.Handle("POST /apps/snapshot", s.signedIn(s.snapshotPost))
		}
	}
	mux.Handle("GET /secrets", s.signedIn(s.secretsPage))
	mux.Handle("POST /secrets/{ns}", s.signedIn(s.secretsPost))
	mux.Handle("GET /models", s.signedIn(s.modelsPage))
	mux.Handle("POST /models", s.signedIn(s.modelsPost))
	if s.Traces != nil {
		mux.Handle("GET /agents", s.signedIn(s.agentsPage))
		mux.Handle("GET /agents/{unit}/{name}", s.signedIn(s.agentsPage))
		mux.Handle("GET /runs/{id}", s.signedIn(s.tracePage))
		mux.Handle("GET /api/runs/{id}", s.signedIn(s.apiTrace))
	}
	mux.Handle("GET /c/{ns}/{repo}/{n}", s.signedIn(s.cardPage))
	mux.Handle("POST /c/{ns}/{repo}/{n}", s.signedIn(s.cardPost))
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
		if errors.Is(err, events.ErrNoAccount) {
			s.firstVisit(w, r, who)
			return
		}
		if err != nil {
			log.Printf("home: %s: %v", who, err)
			http.Error(w, "cannot tell what you may see right now; try again", http.StatusServiceUnavailable)
			return
		}
		h(w, r, v)
	})
}

// firstVisit is the page for someone whose Gitea account is not made yet,
// such as the first root admin right after the setup: Gitea makes it as
// they sign in there, which, signed in to Keycloak already, takes one click.
func (s *Server) firstVisit(w http.ResponseWriter, r *http.Request, who string) {
	back := "https://" + s.Domain + r.URL.RequestURI()
	gitea := "https://git." + s.Domain + "/user/oauth2/keycloak?redirect_to=" + url.QueryEscape(back)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err := page.ExecuteTemplate(w, "first", map[string]string{"Who": who, "Gitea": gitea, "Back": back, "Domain": s.Domain}); err != nil {
		log.Printf("home: first visit: %v", err)
	}
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
	{"lessons", "Lessons"},
	{"problems", "Problems"},
}

func query(r *http.Request, v events.Visibility) (events.Query, url.Values) {
	f := url.Values{}
	q := events.Query{Visibility: v, Limit: 60}
	if ns := r.URL.Query().Get("ns"); ns != "" {
		q.Unit = ns
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
	case "lessons":
		q.Kinds = []string{events.KindLesson}
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
	Cards      []changes.Card // pull requests waiting for this person, or theirs
	Card       *cardView      // the page of one change
	Units      []string
	Views      []struct{ Name, Label string }
	Filter     url.Values
	Query      string // the filter, encoded, for the live updates
	More       int64  // the next page starts below this ID
	Now        time.Time
	Fragment   bool // only the entries, for the live updates and the next page
	Event      *events.Event
	Thread     []events.Event
	Story      *story // what belongs to the event, newest first
	Error      string
	Notice     string       // what the person just did, e.g. a merge
	Secrets    *secretsView // the secrets page
	HasSecrets bool         // the dashboard has a secrets page
	Models     *modelsView  // the Models page
	PeoplePage *peopleView  // the People page
	UnitsPage  *unitsView   // the Units page
	HasUnits   bool         // the dashboard has a Units page
	HasPeople  bool         // the dashboard has a People page
	HasModels  bool         // the dashboard has a Models page, and this is a root admin
	AppsPage   *appsView    // the Apps page
	HasApps    bool         // the dashboard has an Apps page
	Running    *runningView // the agents at work, on the feed
	AgentsPage *agentsView  // the Agents page, or one agent's
	TracePage  *traceView   // one turn of an agent
	HasAgents  bool         // the dashboard has the Agents page
	Working    int          // how many agents work now, in every unit, for the header; -1: unknown
}

func (s *Server) render(w http.ResponseWriter, name string, p feedPage) {
	s.agentNames()
	p.HasSecrets = s.Secrets != nil
	p.HasPeople = s.People != nil && s.Invites != nil
	p.HasUnits = s.Units != nil
	p.HasModels = s.Secrets != nil && s.Providers != nil && p.All
	p.HasApps = s.Apps != nil
	p.HasAgents = s.Traces != nil
	p.Working = -1
	if s.Running != nil && !p.Fragment {
		// The count in the header is everyone's: which units and agents
		// stays with the feed, as each person may see them.
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		if list, err := s.Running.Running(ctx); err == nil {
			p.Working = len(list)
		}
		cancel()
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := page.ExecuteTemplate(w, name, p); err != nil {
		log.Printf("home: render %s: %v", name, err)
	}
}

func (s *Server) feed(w http.ResponseWriter, r *http.Request, v events.Visibility) {
	q, f := query(r, v)
	p := feedPage{Who: v.Person, Domain: s.Domain, All: v.All, Views: views, Filter: f, Query: f.Encode(), Now: s.now(), Notice: mergedNotice(r)}
	es, err := s.Events.List(r.Context(), q)
	if err != nil {
		log.Printf("home: feed: %v", err)
		p.Error = "The feed cannot be read right now: " + err.Error()
	}
	p.Events = es
	if len(es) == q.Limit {
		p.More = es[len(es)-1].ID
	}
	p.Units, _ = s.Events.Units(r.Context(), v)
	s.waitingFor(r.Context(), v, &p)
	p.Running = s.runningFor(r.Context(), v)
	s.render(w, "page", p)
}

// waitingFor fills in what waits for the person: the questions agents asked
// them, still open, and the pull requests waiting for them, or theirs.
func (s *Server) waitingFor(ctx context.Context, v events.Visibility, p *feedPage) {
	p.Waiting, _ = s.Events.List(ctx, events.Query{Visibility: v, Kinds: []string{events.KindQuestion},
		Actor: v.Person, Status: events.StatusWaiting, Limit: 20})
	if s.Changes != nil {
		// The page waits for the changes a few seconds at most: what
		// agents ask, above them, is shown either way.
		ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
		cards, err := s.Changes.Waiting(ctx, v.Person, 10)
		cancel()
		if err != nil {
			log.Printf("home: %s: waiting changes: %v", v.Person, err)
		}
		p.Cards = cards
	}
}

// waiting answers the page's live updates of what waits for the person.
func (s *Server) waiting(w http.ResponseWriter, r *http.Request, v events.Visibility) {
	p := feedPage{Now: s.now(), Fragment: true}
	s.waitingFor(r.Context(), v, &p)
	w.Header().Set("Cache-Control", "no-store")
	s.render(w, "waiting", p)
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
	st := storyOf(e, thread)
	p := feedPage{Who: v.Person, Domain: s.Domain, All: v.All, Event: &e, Thread: thread, Story: &st, Now: s.now(), Views: views}
	if r.URL.Query().Get("part") == "1" {
		// Only the event, for the feed's window over itself.
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		s.agentNames()
		if err := page.ExecuteTemplate(w, "eventbody", p); err != nil {
			log.Printf("home: event %d: %v", id, err)
		}
		return
	}
	s.render(w, "page", p)
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

// shotFile serves a screenshot an agent showed in a chat, to whom may see it
// (events.Visibility.SeesShot); for anyone else, as for one not there, 404.
func (s *Server) shotFile(w http.ResponseWriter, r *http.Request, v events.Visibility) {
	id, ok := strings.CutSuffix(r.PathValue("file"), ".png")
	if !ok || len(id) != 32 {
		http.NotFound(w, r)
		return
	}
	shot, found, err := s.Shots.Shot(r.Context(), id)
	if err != nil {
		http.Error(w, "it cannot be read right now", http.StatusServiceUnavailable)
		return
	}
	if !found || !v.SeesShot(shot) {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "private, max-age=86400")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Write(shot.PNG)
}
