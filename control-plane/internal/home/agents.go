package home

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"garrison/control-plane/internal/events"
)

// Traces reads what agents did, turn by turn: events.Lazy.
type Traces interface {
	Traces(ctx context.Context, q events.TraceQuery) ([]events.Trace, error)
	Trace(ctx context.Context, id int64, v events.Visibility) (events.Trace, bool, error)
}

// agentsView is the Agents page: every agent's recent turns, or one
// agent's.
type agentsView struct {
	Agent  string // unit/name, on one agent's page
	Groups []agentGroup
	Error  string
	Now    time.Time
}

type agentGroup struct {
	Agent string // unit/name
	Runs  []events.Trace
	More  bool // more than shown here
}

// traceView is one turn's page.
type traceView struct {
	Trace events.Trace
	Steps []stepView
	Now   time.Time
}

type stepView struct {
	events.Step
	Args string // a tool call's arguments, indented
}

// perAgent is how many turns the Agents page shows of each agent.
const perAgent = 5

func (s *Server) agentsPage(w http.ResponseWriter, r *http.Request, v events.Visibility) {
	p := feedPage{Who: v.Person, Domain: s.Domain, All: v.All, Now: s.now(), AgentsPage: &agentsView{Now: s.now()}}
	q := events.TraceQuery{Visibility: v, Limit: 200}
	if unit, name := r.PathValue("unit"), r.PathValue("name"); unit != "" {
		p.AgentsPage.Agent = unit + "/" + name
		q.Agent, q.Limit = p.AgentsPage.Agent, 100
	}
	list, err := s.Traces.Traces(r.Context(), q)
	if err != nil {
		log.Printf("home: traces: %v", err)
		p.AgentsPage.Error = "What the agents did cannot be read right now: " + err.Error()
	}
	at := map[string]int{}
	for _, t := range list {
		i, ok := at[t.Agent]
		if !ok {
			i = len(p.AgentsPage.Groups)
			at[t.Agent] = i
			p.AgentsPage.Groups = append(p.AgentsPage.Groups, agentGroup{Agent: t.Agent})
		}
		g := &p.AgentsPage.Groups[i]
		if q.Agent == "" && len(g.Runs) >= perAgent {
			g.More = true
			continue
		}
		g.Runs = append(g.Runs, t)
	}
	s.render(w, "page", p)
}

func (s *Server) tracePage(w http.ResponseWriter, r *http.Request, v events.Visibility) {
	t, ok := s.trace(w, r, v)
	if !ok {
		return
	}
	view := &traceView{Trace: t, Now: s.now()}
	for _, st := range t.Steps {
		sv := stepView{Step: st}
		if st.Kind == events.StepTool {
			var b bytes.Buffer
			if json.Indent(&b, []byte(st.Text), "", "  ") == nil {
				sv.Args = b.String()
			} else {
				sv.Args = st.Text
			}
		}
		view.Steps = append(view.Steps, sv)
	}
	s.render(w, "page", feedPage{Who: v.Person, Domain: s.Domain, All: v.All, Now: s.now(), TracePage: view})
}

func (s *Server) apiTrace(w http.ResponseWriter, r *http.Request, v events.Visibility) {
	if t, ok := s.trace(w, r, v); ok {
		writeJSON(w, t)
	}
}

// trace reads the trace of the request's id, if v sees it; otherwise it
// answers 404, as for one that is not there.
func (s *Server) trace(w http.ResponseWriter, r *http.Request, v events.Visibility) (events.Trace, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return events.Trace{}, false
	}
	t, ok, err := s.Traces.Trace(r.Context(), id, v)
	if err != nil {
		log.Printf("home: trace %d: %v", id, err)
		http.Error(w, "This run cannot be read right now.", http.StatusServiceUnavailable)
		return t, false
	}
	if !ok || !v.SeesTrace(t) {
		http.NotFound(w, r)
		return t, false
	}
	return t, true
}

// where says in words where a turn ran.
func where(t events.Trace) string {
	switch {
	case strings.HasPrefix(t.Chat, "channel:"):
		return "in " + t.Unit + "'s channel"
	case strings.HasPrefix(t.Chat, "routine:"):
		return how(t.StartedBy)
	case strings.HasPrefix(t.StartedBy, "agent "):
		return how(t.StartedBy) + ", in a private chat"
	}
	return "in a private chat"
}

// took is how long a turn ran, or has run so far.
func took(t events.Trace, now time.Time) string {
	end := t.Updated
	if t.Status == events.StatusRunning {
		end = now
	}
	d := end.Sub(t.At).Round(time.Second)
	switch {
	case d < time.Minute:
		return strconv.Itoa(int(d.Seconds())) + " s"
	case d < time.Hour:
		return strconv.Itoa(int(d.Minutes())) + " min"
	}
	return d.Round(time.Minute).String()
}
