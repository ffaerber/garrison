package home

import (
	"context"
	"strings"
	"testing"
	"time"

	"garrison/control-plane/internal/events"
)

// traceStore holds traces in memory and shows each as SeesTrace allows,
// as the database's condition does.
type traceStore []events.Trace

func (s traceStore) Traces(_ context.Context, q events.TraceQuery) ([]events.Trace, error) {
	var out []events.Trace
	for i := len(s) - 1; i >= 0; i-- {
		t := s[i]
		if q.Visibility.SeesTrace(t) && (q.Agent == "" || t.Agent == q.Agent) {
			t.Steps = nil
			out = append(out, t)
		}
	}
	return out, nil
}

func (s traceStore) Trace(_ context.Context, id int64, v events.Visibility) (events.Trace, bool, error) {
	for _, t := range s {
		if t.ID == id && v.SeesTrace(t) {
			return t, true, nil
		}
	}
	return events.Trace{}, false, nil
}

func traceServer() *Server {
	s, _ := server()
	at := time.Now().Add(-time.Minute)
	s.Traces = traceStore{
		{ID: 1, At: at, Updated: at.Add(30 * time.Second), Unit: "todo", Agent: "todo/coder", Person: "bob", Chat: "c-private", StartedBy: "person",
			Status: events.StatusOK, Task: "my private question", Calls: 1, Steps: []events.Step{
				{Kind: events.StepThinking, Text: "reading <secret> repo"},
				{Kind: events.StepTool, Tool: "read_file", Text: `{"repo":"secret","path":"a.txt"}`},
				{Kind: events.StepResult, Tool: "read_file", Text: "the secret"},
				{Kind: events.StepSays, Text: "Done."}}},
		{ID: 2, At: at, Updated: at, Unit: "todo", Agent: "todo/designer", Person: "bob", Chat: "channel:abc", StartedBy: "person", Shared: true,
			Status: events.StatusRunning, Task: "make a short", Steps: []events.Step{{Kind: events.StepTool, Tool: "generate_media", Text: `{"style":"soft"}`}}},
		{ID: 3, At: at, Updated: at, Unit: "garrison", Agent: "garrison/operator", Person: "carol", Chat: "routine:garrison/operator/morning", StartedBy: "routine garrison/operator/morning", Shared: true,
			Status: events.StatusFailed, Task: "look at the logs", Steps: []events.Step{{Kind: events.StepError, Text: "the LLM failed"}}},
	}
	s.Access = access{
		"alice": {All: true},                              // a root admin
		"dave":  {Members: map[string]bool{"todo": true}}, // in todo
		"erin":  {},                                       // in no unit
	}
	return s
}

func TestTracesShowLikeTheAnswer(t *testing.T) {
	s := traceServer()
	sees := func(who string, id string) bool { return get(s, who, "/runs/"+id).Code == 200 }
	cases := []struct {
		who  string
		want map[string]bool
	}{
		{"bob", map[string]bool{"1": true, "2": true, "3": false}},    // his own, private or not
		{"dave", map[string]bool{"1": false, "2": true, "3": false}},  // todo's channel, not bob's private chat
		{"alice", map[string]bool{"1": false, "2": false, "3": true}}, // a root admin: garrison's shared only
		{"erin", map[string]bool{"1": false, "2": false, "3": false}},
	}
	for _, c := range cases {
		for id, want := range c.want {
			if got := sees(c.who, id); got != want {
				t.Errorf("%s sees run %s: %v, want %v", c.who, id, got, want)
			}
		}
	}
	// The list holds the same.
	body := get(s, "dave", "/agents").Body.String()
	if !strings.Contains(body, "make a short") || strings.Contains(body, "my private question") || strings.Contains(body, "look at the logs") {
		t.Errorf("dave's agents page:\n%s", body)
	}
	if get(s, "", "/agents").Code == 200 {
		t.Error("the Agents page without a sign-in")
	}
}

func TestATraceShowsItsSteps(t *testing.T) {
	s := traceServer()
	body := get(s, "bob", "/runs/1").Body.String()
	for _, want := range []string{"my private question", "reading &lt;secret&gt; repo", "read_file", `&#34;repo&#34;: &#34;secret&#34;`, "the secret", "Done.", "for bob", "in a private chat"} {
		if !strings.Contains(body, want) {
			t.Errorf("no %q in the trace page", want)
		}
	}
	if strings.Contains(body, "<secret>") {
		t.Error("the thinking is not escaped")
	}
	running := get(s, "bob", "/runs/2").Body.String()
	if !strings.Contains(running, `http-equiv="refresh"`) || !strings.Contains(running, "in todo&#39;s channel") {
		t.Errorf("a running turn's page:\n%s", running)
	}
	if strings.Contains(body, `http-equiv="refresh"`) {
		t.Error("a finished turn's page refreshes")
	}
	if w := get(s, "bob", "/api/runs/1"); w.Code != 200 || !strings.Contains(w.Body.String(), `"kind":"tool"`) {
		t.Errorf("api: %d %s", w.Code, w.Body.String())
	}
	if get(s, "bob", "/runs/x").Code != 404 || get(s, "bob", "/runs/99").Code != 404 {
		t.Error("a missing run is not 404")
	}
}

func TestOneAgentsPage(t *testing.T) {
	s := traceServer()
	body := get(s, "bob", "/agents/todo/coder").Body.String()
	if !strings.Contains(body, "my private question") || strings.Contains(body, "make a short") || !strings.Contains(body, `href="/runs/1"`) {
		t.Errorf("coder's page:\n%s", body)
	}
	if !strings.Contains(get(s, "bob", "/").Body.String(), `href="/agents"`) {
		t.Error("no Agents in the nav")
	}
	s.Traces = nil
	if get(s, "bob", "/agents").Code == 200 {
		t.Error("an Agents page without traces")
	}
}
