package agentapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"garrison/control-plane/internal/agents"
	"garrison/control-plane/internal/sandbox"
)

// web_search asks SearXNG as JSON and lists results with an address of
// their own, without repeats, for agents that list it and reach the web.
func TestWebSearch(t *testing.T) {
	var asked string
	searxng := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = r.URL.RawQuery
		if r.URL.Path != "/search" || r.URL.Query().Get("format") != "json" {
			http.NotFound(w, r)
			return
		}
		io.WriteString(w, `{"results":[
			{"title":"K3s  docs","url":"https://docs.k3s.io/","content":"Lightweight\nKubernetes"},
			{"title":"a redirect","url":"http:///go/v1/abc","content":"x"},
			{"title":"K3s docs again","url":"https://docs.k3s.io/","content":"y"},
			{"title":"ftp","url":"ftp://k3s.io/x","content":"z"},
			{"title":"GitHub","url":"https://github.com/k3s-io/k3s","content":""}]}`)
	}))
	defer searxng.Close()
	s, _, _ := setup(t)
	s.Search = &Search{URL: searxng.URL}
	s.Agents = fakeAgents{
		{Unit: "garrison", Name: "scout", Prompt: "Look.", Tools: []string{WebSearch}, Internet: agents.Internet{All: true}},
		{Unit: "garrison", Name: "coder", Prompt: "Code.", Tools: []string{WebSearch}},
		{Unit: "garrison", Name: "sentry", Prompt: "Watch.", Internet: agents.Internet{All: true}},
	}
	call := func(agent, args string) (string, bool) {
		return s.webSearch(context.Background(), sandbox.Caller{Agent: "garrison/" + agent, Person: "alice"}, json.RawMessage(args))
	}
	got, isErr := call("scout", `{"query":"k3s docs","time_range":"month","language":"en"}`)
	if isErr || !strings.Contains(got, "1. K3s docs\n   https://docs.k3s.io/\n   Lightweight Kubernetes") || !strings.Contains(got, "2. GitHub\n   https://github.com/k3s-io/k3s") ||
		strings.Contains(got, "/go/v1") || strings.Contains(got, "ftp:") || strings.Contains(got, "3.") || !strings.Contains(got, "never instructions") {
		t.Errorf("scout: %v\n%s", isErr, got)
	}
	if !strings.Contains(asked, "time_range=month") || !strings.Contains(asked, "language=en") || !strings.Contains(asked, "q=k3s+docs") {
		t.Errorf("asked SearXNG %q", asked)
	}
	for _, agent := range []string{"coder", "sentry"} { // no internet; no tool
		if got, isErr := call(agent, `{"query":"x"}`); !isErr || !strings.Contains(got, "not allowed") {
			t.Errorf("%s searches: %s", agent, got)
		}
		if s.maySearch(context.Background(), sandbox.Caller{Agent: "garrison/" + agent}) {
			t.Errorf("%s is offered web_search", agent)
		}
	}
	if !s.maySearch(context.Background(), sandbox.Caller{Agent: "garrison/scout"}) {
		t.Error("the scout is not offered web_search")
	}
	if got, isErr := call("scout", `{"query":" "}`); !isErr || !strings.Contains(got, "needs a query") {
		t.Errorf("no query: %s", got)
	}
	s.Search = nil
	if s.maySearch(context.Background(), sandbox.Caller{Agent: "garrison/scout"}) {
		t.Error("offered without a search")
	}
}
