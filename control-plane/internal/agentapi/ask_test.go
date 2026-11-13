package agentapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"homecloud/control-plane/internal/agents"
	"homecloud/control-plane/internal/gateway"
	"homecloud/control-plane/internal/sandbox"
)

type fakePoster struct {
	mu       sync.Mutex
	posts    []gateway.Post
	channels map[string]gateway.ChannelInfo
}

func (f *fakePoster) Channel(_ context.Context, id string) (gateway.ChannelInfo, error) {
	return f.channels[id], nil
}

func (f *fakePoster) Post(_ context.Context, p gateway.Post) error {
	f.mu.Lock()
	f.posts = append(f.posts, p)
	f.mu.Unlock()
	return nil
}

func (f *fakePoster) all() []gateway.Post {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]gateway.Post(nil), f.posts...)
}

func withTeam(s *Server) {
	s.Agents = fakeAgents{
		{Namespace: "homepage", Name: "lead", Prompt: "Lead.", Asks: []string{"homepage/tester"}},
		{Namespace: "homepage", Name: "tester", Prompt: "Test."},
		{Namespace: "homepage", Name: "loner", Prompt: "Alone."},
	}
}

func TestAnAgentAsksAnother(t *testing.T) {
	s, _, _ := setup(t)
	withTeam(s)
	d := withSandboxes(t, s)
	reasoning, content := chatIn(t, s, "chat-1", "homepage/lead", Message{Role: "user",
		Content: `call ask_agent {"agent":"homepage/tester","task":"Check the login page.","limits":"Read only.","done_when":"It says whether it works."}`})
	for _, want := range []string{"called: homepage/tester answers:", "echo: homepage/lead asks you, working for alice.", "Task: Check the login page.", "Limits: Read only.", "A good answer: It says whether it works."} {
		if !strings.Contains(content, want) {
			t.Errorf("content lacks %q: %s", want, content)
		}
	}
	if !strings.Contains(reasoning, "[homepage/tester] hmm") {
		t.Errorf("the asked agent's progress does not show: %q", reasoning)
	}
	if d.Created != 2 {
		t.Errorf("%d sandboxes; want the lead's and the tester's", d.Created)
	}
	// The lead lists ask_agent among its tools; an agent that asks nobody does not.
	_, content = chatIn(t, s, "chat-1", "homepage/lead", Message{Role: "user", Content: "which tools?"})
	if content != "tools: 2" { // read_file from the gateway, and ask_agent
		t.Errorf("lead's tools: %q", content)
	}
	_, content = chatIn(t, s, "chat-2", "homepage/loner", Message{Role: "user", Content: `call ask_agent {"agent":"homepage/tester","task":"x","done_when":"y"}`})
	if !strings.Contains(content, "error: not allowed: you may ask only") {
		t.Errorf("loner asked: %q", content)
	}
}

func TestAskingHasLimits(t *testing.T) {
	s, _, _ := setup(t)
	withTeam(s)
	list := s.Agents.(fakeAgents)
	list[1].Asks = []string{"homepage/lead"}
	ask := func(c sandbox.Caller, args string) string {
		text, _ := s.askAgent(context.Background(), c, json.RawMessage(args))
		return text
	}
	c := sandbox.Caller{Agent: "homepage/tester", Chat: "chat-1", Person: "alice", Run: sandbox.Run{Chain: []string{"homepage/lead", "homepage/tester"}}}
	if got := ask(c, `{"agent":"homepage/lead","task":"x","done_when":"y"}`); !strings.Contains(got, "already part of this run") {
		t.Errorf("a cycle: %q", got)
	}
	c = sandbox.Caller{Agent: "homepage/lead", Chat: "chat-1", Person: "alice", Run: sandbox.Run{Chain: []string{"a/x", "a/y", "homepage/lead"}}}
	if got := ask(c, `{"agent":"homepage/tester","task":"x","done_when":"y"}`); !strings.Contains(got, "the most there may be") {
		t.Errorf("too deep: %q", got)
	}
	c = sandbox.Caller{Agent: "homepage/lead", Chat: "chat-1", Person: "alice", Run: sandbox.Run{Chain: []string{"homepage/lead"}}}
	if got := ask(c, `{"agent":"homepage/tester","task":"x","done_when":"y","background":true}`); !strings.Contains(got, "only in a channel") {
		t.Errorf("background outside a channel: %q", got)
	}
}

func TestBackgroundAnswersArePosted(t *testing.T) {
	s, _, _ := setup(t)
	withTeam(s)
	withSandboxes(t, s)
	poster := &fakePoster{}
	s.Poster = poster
	_, content := chatIn(t, s, "channel:c1", "homepage/lead", Message{Role: "user", Content: `call ask_agent {"agent":"homepage/tester","task":"Check it.","done_when":"y","background":true}`})
	if !strings.Contains(content, "will post its answer in this channel") {
		t.Fatalf("content %q", content)
	}
	deadline := time.Now().Add(5 * time.Second)
	for len(poster.all()) == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	posts := poster.all()
	if len(posts) != 1 || posts[0].Agent != "homepage/tester" || posts[0].Channel != "c1" || !strings.Contains(posts[0].Text, "For alice, asked by homepage/lead:") {
		t.Errorf("posts: %+v", posts)
	}
}

func TestRoutines(t *testing.T) {
	s, _, mcp := setup(t)
	head := "c1"
	gitea := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/repos/homepage/config/raw/members.yaml":
			io.WriteString(w, "admins: [alice]\nmembers: [bob]\n")
		case "/api/v1/repos/homepage/todo/branches/main":
			io.WriteString(w, `{"commit":{"id":"`+head+`"}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer gitea.Close()
	g := &gateway.Gitea{URL: gitea.URL, User: "admin", Password: "pw"}
	s.Agents = fakeAgents{{Namespace: "homepage", Name: "dev", Prompt: "Dev.", Tools: []string{"list_repos"}, Routines: []agents.Routine{
		{Name: "hourly", Every: time.Hour, Raw: "1h", Owner: "bob", Task: "call list_repos {}"},
		{Name: "on-main", OnPush: &agents.Push{Repo: "todo", Branch: "main"}, Owner: "bob", Task: "Look at main."},
		{Name: "orphan", Every: time.Hour, Raw: "1h", Owner: "mallory", Task: "x"},
	}}}
	d := withSandboxes(t, s)
	s.Gateway.ServiceKey = "k"
	poster := &fakePoster{}
	s.Poster = poster
	now := time.Date(2026, 11, 13, 8, 0, 0, 0, time.UTC)
	r := &Routines{Server: s, Gitea: g, Now: func() time.Time { return now }}
	wait := func(n int) []gateway.Post {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for len(poster.all()) < n && time.Now().Before(deadline) {
			time.Sleep(20 * time.Millisecond)
		}
		time.Sleep(50 * time.Millisecond)
		return poster.all()
	}
	r.Check(context.Background()) // first seen: nothing runs
	if got := wait(0); len(got) != 0 || d.Created != 0 {
		t.Fatalf("ran at once: %+v", got)
	}
	now = now.Add(30 * time.Minute)
	head = "c2"
	r.Check(context.Background()) // main moved
	posts := wait(1)
	// todo is unknown to this Gitea, so private: the post goes to the team channel.
	if len(posts) != 1 || !strings.Contains(posts[0].Text, "Routine on-main, for bob (main of homepage/todo moved to c2)") || posts[0].Channel != "" || !posts[0].Team {
		t.Fatalf("on push: %+v", posts)
	}
	now = now.Add(31 * time.Minute)
	r.Check(context.Background()) // an hour on
	posts = wait(3)
	var hourly, orphan string
	for _, p := range posts[1:] {
		if strings.Contains(p.Text, "Routine hourly") {
			hourly = p.Text
		}
		if strings.Contains(p.Text, "orphan") {
			orphan = p.Text
		}
	}
	if !strings.Contains(hourly, "called: ") {
		t.Errorf("hourly: %q", hourly)
	}
	if !strings.Contains(orphan, "did not run: its owner mallory is not in homepage") {
		t.Errorf("orphan: %q", orphan)
	}
	// The routine's tool call went to the gateway without a token, for bob,
	// saying what started it.
	if !mcp.sawUnattended("bob", "routine homepage/dev/hourly") {
		t.Errorf("the gateway was not told: %v", mcp.headers())
	}
}

func TestPrivateAgentsAnswerOnlyAmongTheirPeople(t *testing.T) {
	s, _, _ := setup(t)
	gitea := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/repos/homepage/config/raw/members.yaml":
			io.WriteString(w, "admins: [alice]\n")
		case "/api/v1/repos/homepage/secret":
			io.WriteString(w, `{"private":true}`)
		case "/api/v1/repos/homepage/site":
			io.WriteString(w, `{"private":false}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer gitea.Close()
	s.Gitea = &gateway.Gitea{URL: gitea.URL, User: "admin", Password: "pw"}
	s.Poster = &fakePoster{channels: map[string]gateway.ChannelInfo{
		"pub": {Namespace: "homepage"}, "team": {Namespace: "homepage", Team: true}, "other-team": {Namespace: "shop", Team: true},
	}}
	s.Agents = fakeAgents{
		{Namespace: "homepage", Name: "keeper", Prompt: "Keep.", Repos: []string{"secret"}},
		{Namespace: "homepage", Name: "desk", Prompt: "Desk.", Repos: []string{"site"}},
	}
	withSandboxes(t, s)
	for _, c := range []struct {
		agent, chat string
		code        int
	}{
		{"homepage/keeper", "channel:pub", http.StatusForbidden},
		{"homepage/keeper", "channel:other-team", http.StatusForbidden},
		{"homepage/keeper", "channel:team", http.StatusOK},
		{"homepage/keeper", "chat-1", http.StatusOK},
		{"homepage/desk", "channel:pub", http.StatusOK},
	} {
		w := post(t, s, "tok-alice", map[string]string{ChatHeader: c.chat}, map[string]any{"model": c.agent, "stream": true, "messages": []Message{{Role: "user", Content: "hi"}}})
		if w.Code != c.code {
			t.Errorf("%s in %s: %d, want %d: %s", c.agent, c.chat, w.Code, c.code, w.Body)
		}
		if c.code == http.StatusForbidden && !strings.Contains(w.Body.String(), "homepage-team") {
			t.Errorf("the refusal does not say where: %s", w.Body)
		}
	}
}
