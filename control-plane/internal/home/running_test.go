package home

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"garrison/control-plane/internal/gateway"
	"garrison/control-plane/internal/sandbox"
)

type fixedRunning []sandbox.Running

func (f fixedRunning) Running(context.Context) ([]sandbox.Running, error) { return f, nil }

type brokenRunning struct{}

func (brokenRunning) Running(context.Context) ([]sandbox.Running, error) {
	return nil, errors.New("no agent service")
}

func runningServer() *Server {
	s := appsServer()
	since := time.Now().Add(-2 * time.Minute)
	s.Running = fixedRunning{
		{Unit: "lab", Agent: "coder", For: "erin", StartedBy: "routine lab/coder/news-daily", Chain: []string{"lab/coder"}, Since: since},
		{Unit: "lab", Agent: "scout", For: "erin", StartedBy: "agent lab/coder", Chain: []string{"lab/coder", "lab/scout"}, Since: since},
		{Unit: "todo", Agent: "operator", For: "frank", StartedBy: "person", Chain: []string{"todo/operator"}, Since: since},
	}
	return s
}

func apiRunning(t *testing.T, s *Server, who string) runningView {
	t.Helper()
	w := get(s, who, "/api/running")
	if w.Code != 200 {
		t.Fatalf("%s: /api/running: %d %s", who, w.Code, w.Body)
	}
	var v runningView
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func agentsOf(v runningView) string {
	var n []string
	for _, r := range v.Shown {
		n = append(n, r.Unit+"/"+r.Agent+" for "+r.For+" "+r.How)
	}
	return strings.Join(n, "; ")
}

// Everyone learns how many work; what and for whom, only the unit's
// people, the person it works for and root admins, as in the feed.
func TestRunningAsEachMaySee(t *testing.T) {
	s := runningServer()
	for who, want := range map[string]struct {
		agents string
		hidden int
	}{
		"root":  {"lab/coder for erin routine news-daily; lab/scout for erin asked by lab/coder; todo/operator for frank in a chat", 0},
		"erin":  {"lab/coder for erin routine news-daily; lab/scout for erin asked by lab/coder", 1},
		"frank": {"todo/operator for frank in a chat", 2},
		"dave":  {"", 3}, // reads todo's repos, is in no unit, and nothing works for him
	} {
		v := apiRunning(t, s, who)
		if v.Total != 3 || v.Units != 2 || agentsOf(v) != want.agents || v.Hidden != want.hidden {
			t.Errorf("%s: total %d in %d units, sees %q (want %q), hidden %d (want %d)", who, v.Total, v.Units, agentsOf(v), want.agents, v.Hidden, want.hidden)
		}
	}
}

func TestRunningOnTheDashboard(t *testing.T) {
	s := runningServer()
	body := get(s, "erin", "/").Body.String()
	for _, want := range []string{`3 agents working`, `Working now: 3 agents in 2 units`, `data-run="lab/scout"`, "lab/coder → lab/scout", "asked by lab/coder", "and 1 more in units you are not in"} {
		if !strings.Contains(body, want) {
			t.Errorf("erin's feed lacks %q", want)
		}
	}
	if strings.Contains(body, "operator") && strings.Contains(body, `data-run="todo/operator"`) {
		t.Error("erin sees what works in todo")
	}
	// Every page has the count in its header.
	if body := get(s, "dave", "/apps").Body.String(); !strings.Contains(body, "3 agents working") {
		t.Error("the Apps page has no count of agents working")
	}
	// The section alone, for its live updates.
	w := get(s, "erin", "/running")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `data-run="lab/coder"`) || strings.Contains(w.Body.String(), "<html") {
		t.Errorf("/running: %d %s", w.Code, w.Body)
	}
	// Nothing at work: no section, and the header says so.
	s.Running = fixedRunning{}
	body = get(s, "erin", "/").Body.String()
	if strings.Contains(body, "Working now") || !strings.Contains(body, "no agent working") {
		t.Error("with nobody working, the feed still shows the section, or the header no count")
	}
}

func TestRunningWithoutTheAgentService(t *testing.T) {
	s := runningServer()
	s.Running = brokenRunning{}
	if w := get(s, "erin", "/api/running"); w.Code != http.StatusServiceUnavailable {
		t.Errorf("/api/running: %d", w.Code)
	}
	if w := get(s, "erin", "/"); w.Code != 200 || strings.Contains(w.Body.String(), "agents working") {
		t.Errorf("the feed without the agent service: %d", w.Code)
	}
	s.Running = nil
	if w := get(s, "erin", "/api/running"); w.Code != http.StatusNotFound {
		t.Errorf("/api/running without Running: %d", w.Code)
	}
}

// RemoteRunning asks with the service key, which the agent service checks.
func TestRemoteRunning(t *testing.T) {
	agentService := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/internal/running" || r.Header.Get(gateway.ServiceHeader) != "key" {
			http.Error(w, "no", http.StatusForbidden)
			return
		}
		w.Write([]byte(`[{"unit":"lab","agent":"coder","for":"erin","started_by":"person","chain":["lab/coder"],"since":"2026-11-24T12:00:00Z"}]`))
	}))
	defer agentService.Close()
	list, err := RemoteRunning(agentService.URL, "key").Running(context.Background())
	if err != nil || len(list) != 1 || list[0].Agent != "coder" || list[0].For != "erin" {
		t.Errorf("running: %v %+v", err, list)
	}
	if _, err := RemoteRunning(agentService.URL, "wrong").Running(context.Background()); err == nil {
		t.Error("a wrong key is not refused")
	}
}
