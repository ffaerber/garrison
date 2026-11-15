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

	"garrison/control-plane/internal/agents"
	"garrison/control-plane/internal/cron"
	"garrison/control-plane/internal/events"
	"garrison/control-plane/internal/gateway"
	"garrison/control-plane/internal/sandbox"
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
	if content != "tools: 3" { // read_file from the gateway, ask_agent and find_agents
		t.Errorf("lead's tools: %q", content)
	}
	_, content = chatIn(t, s, "chat-2", "homepage/loner", Message{Role: "user", Content: `call ask_agent {"agent":"homepage/tester","task":"x","done_when":"y"}`})
	if !strings.Contains(content, "error: not allowed: you may ask only") {
		t.Errorf("loner asked: %q", content)
	}
}

func TestTheCommanderFindsAndHandsOver(t *testing.T) {
	s, _, _ := setup(t)
	s.Agents = fakeAgents{
		{Namespace: "system", Name: "commander", Prompt: "Command.", Asks: []string{agents.AnyAgent}},
		{Namespace: "homepage", Name: "media", Description: "Makes screenshots", Prompt: "Shoot.", Tools: []string{"screenshot"}, Repos: []string{"site"},
			Skills: []agents.Skill{{Name: "thumbs", Description: "thumbnails"}},
			Grants: []agents.Grant{{Repo: "site", Branch: "dev", Level: agents.LevelAutonomous, For: agents.ForMembers}}},
		{Namespace: "todo", Name: "operator", Description: "Runs todo", Prompt: "Run."},
	}
	feed := &events.Memory{}
	s.Events = feed
	withSandboxes(t, s)
	find := func(c sandbox.Caller, args string) string {
		text, _ := s.findAgents(context.Background(), c, json.RawMessage(args))
		return text
	}
	c := sandbox.Caller{Agent: "system/commander", Chat: "chat-1", Person: "alice", Run: sandbox.Run{Chain: []string{"system/commander"}}}
	got := find(c, `{}`)
	for _, want := range []string{"- homepage/media: Makes screenshots", "tools: screenshot", "skills: thumbs (thumbnails)", "works on: site", "may autonomous on site into dev, for members", "- todo/operator: Runs todo"} {
		if !strings.Contains(got, want) {
			t.Errorf("find_agents lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "system/commander") {
		t.Errorf("the commander lists itself:\n%s", got)
	}
	if got := find(c, `{"namespace":"todo"}`); strings.Contains(got, "homepage/media") || !strings.Contains(got, "todo/operator") {
		t.Errorf("by namespace:\n%s", got)
	}
	if got := find(c, `{"namespace":"nope"}`); !strings.Contains(got, "no agent you may ask") {
		t.Errorf("none: %q", got)
	}
	// An agent with a list finds only those on it.
	lead := sandbox.Caller{Agent: "homepage/lead", Chat: "chat-1", Person: "alice"}
	s.Agents = append(s.Agents.(fakeAgents), agents.Agent{Namespace: "homepage", Name: "lead", Prompt: "Lead.", Asks: []string{"todo/operator"}})
	if got := find(lead, `{}`); strings.Contains(got, "homepage/media") || !strings.Contains(got, "todo/operator") {
		t.Errorf("the lead finds:\n%s", got)
	}

	// It asks any agent, and the feed has the hand-off and its end.
	_, content := chatIn(t, s, "chat-1", "system/commander", Message{Role: "user",
		Content: `call ask_agent {"agent":"homepage/media","task":"Screenshot the homepage.","done_when":"A pull request with it."}`})
	if !strings.Contains(content, "homepage/media answers:") {
		t.Fatalf("content %q", content)
	}
	var ask *events.Event
	for _, e := range feed.All() {
		if e.Kind == events.KindAsk {
			ask = &e
		}
	}
	if ask == nil || ask.Status != events.StatusOK || ask.Namespace != "homepage" || ask.Agent != "homepage/media" || ask.Actor != "alice" ||
		ask.StartedBy != "agent system/commander" || !strings.Contains(ask.Title, "system/commander asked homepage/media, for alice: Screenshot the homepage.") {
		t.Errorf("feed: %+v", ask)
	}
	if _, content = chatIn(t, s, "chat-1", "system/commander", Message{Role: "user", Content: `call ask_agent {"agent":"system/commander","task":"x","done_when":"y"}`}); !strings.Contains(content, "you do not ask yourself") {
		t.Errorf("asked itself: %q", content)
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
	if len(posts) != 1 || !strings.Contains(posts[0].Text, "Routine on-main, for bob (main of homepage/todo moved to c2)") || posts[0].Channel != "" {
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
		"homepage": {Namespace: "homepage", Members: true}, "system": {Namespace: "system"}, "shop": {Namespace: "shop", Members: true},
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
		{"homepage/keeper", "channel:system", http.StatusForbidden}, // everyone reads it
		{"homepage/keeper", "channel:shop", http.StatusForbidden},   // shop's people do
		{"homepage/keeper", "channel:homepage", http.StatusOK},
		{"homepage/keeper", "chat-1", http.StatusOK},
		{"homepage/desk", "channel:system", http.StatusOK},
	} {
		w := post(t, s, "tok-alice", map[string]string{ChatHeader: c.chat}, map[string]any{"model": c.agent, "stream": true, "messages": []Message{{Role: "user", Content: "hi"}}})
		if w.Code != c.code {
			t.Errorf("%s in %s: %d, want %d: %s", c.agent, c.chat, w.Code, c.code, w.Body)
		}
		if c.code == http.StatusForbidden && !strings.Contains(w.Body.String(), "ask it in the channel homepage or in a private chat") {
			t.Errorf("the refusal does not say where: %s", w.Body)
		}
	}
}

func TestAnAskedAgentsAnswerIsItsLastMessage(t *testing.T) {
	var shown strings.Builder
	c := &collect{show: func(s string) { shown.WriteString(s) }, prefix: "[x] "}
	c.content("Let me ")
	c.content("look.")
	c.step() // that was before a tool call
	c.content("\n\nIt works.")
	if c.answer() != "It works." || shown.String() != "[x] Let me look.\n" {
		t.Errorf("answer %q, shown %q", c.answer(), shown.String())
	}
}

// memState is a RoutineStore in memory, standing in for Postgres.
type memState struct {
	mu sync.Mutex
	m  map[string]RoutineState
}

func (s *memState) RoutineStates(context.Context) (map[string]RoutineState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]RoutineState{}
	for k, v := range s.m {
		out[k] = v
	}
	return out, nil
}

func (s *memState) DeleteRoutineState(_ context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, key)
	return nil
}

func (s *memState) SaveRoutineState(_ context.Context, key string, v RoutineState) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m == nil {
		s.m = map[string]RoutineState{}
	}
	s.m[key] = v
	return nil
}

func TestSchedulesAndRestarts(t *testing.T) {
	s, _, _ := setup(t)
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
	morning, err := cron.Parse("30 6 * * *")
	if err != nil {
		t.Fatal(err)
	}
	s.Agents = fakeAgents{{Namespace: "homepage", Name: "dev", Prompt: "Dev.", Routines: []agents.Routine{
		{Name: "morning", Schedule: "30 6 * * *", Cron: morning, Owner: "bob", Task: "Sum up."},
		{Name: "on-main", OnPush: &agents.Push{Repo: "todo", Branch: "main"}, Owner: "bob", Task: "Look at main."},
	}}}
	withSandboxes(t, s)
	poster := &fakePoster{}
	s.Poster = poster
	feed := &events.Memory{}
	s.Events = feed
	berlin, _ := time.LoadLocation("Europe/Berlin")
	now := time.Date(2026, 11, 13, 22, 0, 0, 0, berlin)
	state := &memState{}
	start := func() *Routines {
		return &Routines{Server: s, Gitea: g, Store: state, Location: berlin, Now: func() time.Time { return now }}
	}
	posts := func(n int) []gateway.Post {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for len(poster.all()) < n && time.Now().Before(deadline) {
			time.Sleep(20 * time.Millisecond)
		}
		time.Sleep(50 * time.Millisecond)
		return poster.all()
	}
	r := start()
	r.Check(context.Background()) // first seen
	now = now.Add(8 * time.Hour)  // 06:00: not yet
	r.Check(context.Background())
	if got := posts(0); len(got) != 0 {
		t.Fatalf("ran early: %+v", got)
	}

	// The agent service restarts; meanwhile 06:30 passes and main moves.
	r = start()
	now = now.Add(45 * time.Minute)
	head = "c2"
	r.Check(context.Background())
	got := posts(2)
	var texts []string
	for _, p := range got {
		texts = append(texts, p.Text)
	}
	all := strings.Join(texts, "\n")
	if len(got) != 2 || !strings.Contains(all, "Routine morning, for bob (it is scheduled for Sat 14 Nov 06:30 (30 6 * * *, Europe/Berlin))") ||
		!strings.Contains(all, "Routine on-main, for bob (main of homepage/todo moved to c2)") {
		t.Fatalf("after the restart: %s", all)
	}
	// Once each: a later look, the same day, starts nothing.
	now = now.Add(2 * time.Hour)
	r.Check(context.Background())
	if got := posts(3); len(got) != 2 {
		t.Errorf("ran again: %+v", got)
	}
	// The feed has each run, ended.
	runs := 0
	for _, e := range feed.All() {
		if e.Kind == events.KindRoutine && e.Status == events.StatusOK && e.Actor == "bob" {
			runs++
		}
	}
	if runs != 2 {
		t.Errorf("feed: %+v", feed.All())
	}
}

func TestAlertsStartRoutines(t *testing.T) {
	s, _, _ := setup(t)
	gitea := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/repos/homepage/config/raw/members.yaml" {
			io.WriteString(w, "admins: [alice]\nmembers: [bob]\n")
			return
		}
		http.NotFound(w, r)
	}))
	defer gitea.Close()
	s.Agents = fakeAgents{{Namespace: "homepage", Name: "dev", Prompt: "Dev.", Routines: []agents.Routine{
		{Name: "down", OnAlert: map[string]string{"alertname": "ServiceDown"}, Owner: "bob", Task: "Find out why."},
		{Name: "disk", OnAlert: map[string]string{"alertname": "DiskFull"}, Owner: "bob", Task: "Free some space."},
	}}}
	withSandboxes(t, s)
	poster := &fakePoster{}
	s.Poster = poster
	feed := &events.Memory{}
	s.Events = feed
	state := &memState{}
	r := &Routines{Server: s, Gitea: &gateway.Gitea{URL: gitea.URL, User: "admin", Password: "pw"}, Store: state}
	h := r.AlertHandler("tok")
	send := func(token, body string) int {
		req := httptest.NewRequest("POST", "/alerts", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w.Code
	}
	alert := func(ns, status string) string {
		return `{"alerts":[{"status":"` + status + `","fingerprint":"f1","startsAt":"2026-11-14T10:00:00Z",
 "labels":{"alertname":"ServiceDown","namespace":"` + ns + `","app":"quotes","service":"quotes_web"},
 "annotations":{"summary":"quotes_web does not run","description":"0 of 1 tasks run."}}]}`
	}
	if code := send("wrong", alert("homepage", "firing")); code != http.StatusUnauthorized {
		t.Fatalf("a wrong token: %d", code)
	}
	if code := send("tok", alert("homepage", "firing")); code != http.StatusNoContent {
		t.Fatalf("firing: %d", code)
	}
	deadline := time.Now().Add(5 * time.Second)
	for len(poster.all()) == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	posts := poster.all()
	if len(posts) != 1 || !strings.Contains(posts[0].Text, "Routine down, for bob (the alert ServiceDown fired at Sat 14 Nov 10:00: quotes_web does not run") {
		t.Fatalf("posts: %+v", posts)
	}
	// Alertmanager sends it again: it started its routine before.
	send("tok", alert("homepage", "firing"))
	// Another namespace's alert starts nothing here.
	send("tok", strings.Replace(alert("shop", "firing"), `"f1"`, `"f2"`, 1))
	time.Sleep(200 * time.Millisecond)
	if n := len(poster.all()); n != 1 {
		t.Errorf("%d posts", n)
	}
	if _, ok := state.m["homepage/dev/down#alert:f1:1794650400"]; !ok {
		t.Errorf("not kept: %v", state.m)
	}
	send("tok", alert("homepage", "resolved"))
	var mine []events.Event
	for _, e := range feed.All() {
		if e.Kind == events.KindAlert && e.Namespace == "homepage" {
			mine = append(mine, e)
		}
	}
	if len(mine) != 1 || mine[0].Status != events.StatusOK || mine[0].Title != "resolved: ServiceDown: quotes_web does not run" ||
		mine[0].Audience != events.AudienceMembers || !strings.Contains(mine[0].Detail, "0 of 1 tasks run.") {
		t.Errorf("feed: %+v", mine)
	}
	ran := false
	for _, e := range feed.All() {
		ran = ran || e.Kind == events.KindRoutine && strings.Contains(e.Title, "the alert ServiceDown fired")
	}
	if !ran {
		t.Errorf("the run is not in the feed: %+v", feed.All())
	}
}

// A push that comes while the routine still runs for an earlier one is not
// lost: it starts the routine once that run is over.
func TestAPushDuringARunStartsTheRoutineAfterIt(t *testing.T) {
	s, _, _ := setup(t)
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
	s.Agents = fakeAgents{{Namespace: "homepage", Name: "dev", Prompt: "Dev.", Routines: []agents.Routine{
		{Name: "on-main", OnPush: &agents.Push{Repo: "todo", Branch: "main"}, Owner: "bob", Task: "Look at main."},
	}}}
	withSandboxes(t, s)
	poster := &fakePoster{}
	s.Poster = poster
	state := &memState{}
	r := &Routines{Server: s, Gitea: &gateway.Gitea{URL: gitea.URL, User: "admin", Password: "pw"}, Store: state}
	const key = "homepage/dev/on-main"
	busy := func(yes bool) {
		r.mu.Lock()
		r.running[key] = yes
		r.mu.Unlock()
	}
	ctx := context.Background()
	r.Check(ctx) // first seen, at c1
	busy(true)   // a run for an earlier push is still going
	head = "c2"
	r.Check(ctx)
	if got := state.m[key].Head; got != "c1" {
		t.Errorf("the push was marked as seen (%s) though nothing ran for it", got)
	}
	busy(false)
	r.Check(ctx)
	deadline := time.Now().Add(5 * time.Second)
	for len(poster.all()) == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	posts := poster.all()
	if len(posts) != 1 || !strings.Contains(posts[0].Text, "moved to c2") {
		t.Fatalf("the push during the run started nothing: %+v", posts)
	}
	if got := state.m[key].Head; got != "c2" {
		t.Errorf("kept head %s, want c2", got)
	}
}

// ctxPoster posts only while its context lasts, as the real one does.
type ctxPoster struct{ fakePoster }

func (f *ctxPoster) Post(ctx context.Context, p gateway.Post) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return f.fakePoster.Post(ctx, p)
}

// A background answer is posted even when the asked agent ran out of time:
// the person was told it would come.
func TestABackgroundAnswerIsPostedWhenTheTurnTimesOut(t *testing.T) {
	s, _, _ := setup(t)
	withTeam(s)
	withSandboxes(t, s)
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(700 * time.Millisecond)
		io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"late"}]}}`)
	}))
	defer slow.Close()
	s.Gateway = &Gateway{URL: slow.URL}
	s.TurnTimeout = 200 * time.Millisecond
	poster := &ctxPoster{}
	s.Poster = poster
	c := sandbox.Caller{Agent: "homepage/lead", Chat: "channel:c1", Person: "alice", Run: sandbox.Run{Chain: []string{"homepage/lead"}}}
	text, isErr := s.askAgent(context.Background(), c, json.RawMessage(`{"agent":"homepage/tester","task":"call list_repos {}","done_when":"y","background":true}`))
	if isErr || !strings.Contains(text, "will post its answer in this channel") {
		t.Fatalf("asked: %q", text)
	}
	deadline := time.Now().Add(5 * time.Second)
	for len(poster.all()) == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	posts := poster.all()
	if len(posts) != 1 || !strings.Contains(posts[0].Text, "It stopped: ") {
		t.Fatalf("nothing was posted after the turn timed out: %+v", posts)
	}
}

// A firing's mark, which keeps it from starting its routine twice, is kept
// while Alertmanager still sends the alert, and forgotten a week after.
func TestAFiringIsForgottenAWeekAfterItWasLastSent(t *testing.T) {
	s, _, _ := setup(t)
	gitea := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/repos/homepage/config/raw/members.yaml" {
			io.WriteString(w, "admins: [alice]\nmembers: [bob]\n")
			return
		}
		http.NotFound(w, r)
	}))
	defer gitea.Close()
	s.Agents = fakeAgents{{Namespace: "homepage", Name: "dev", Prompt: "Dev.", Routines: []agents.Routine{
		{Name: "down", OnAlert: map[string]string{"alertname": "ServiceDown"}, Owner: "bob", Task: "Find out why."},
	}}}
	withSandboxes(t, s)
	poster := &fakePoster{}
	s.Poster = poster
	state := &memState{}
	var clock sync.Mutex
	now := time.Date(2026, 11, 14, 10, 0, 0, 0, time.UTC)
	later := func(d time.Duration) {
		clock.Lock()
		now = now.Add(d)
		clock.Unlock()
	}
	r := &Routines{Server: s, Gitea: &gateway.Gitea{URL: gitea.URL, User: "admin", Password: "pw"}, Store: state,
		Now: func() time.Time { clock.Lock(); defer clock.Unlock(); return now }}
	firing := Alert{Status: "firing", Fingerprint: "f1", StartsAt: now, Labels: map[string]string{"alertname": "ServiceDown", "namespace": "homepage"}}
	const mark = "homepage/dev/down#alert:f1:1794650400"
	kept := func() bool {
		state.mu.Lock()
		defer state.mu.Unlock()
		_, ok := state.m[mark]
		return ok
	}
	ctx := context.Background()
	r.Alert(ctx, firing)
	deadline := time.Now().Add(5 * time.Second)
	for len(poster.all()) == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if !kept() {
		t.Fatal("the firing was not marked")
	}
	// Still firing five days on: sent again, it starts nothing, and stays.
	later(5 * 24 * time.Hour)
	r.Alert(ctx, firing)
	later(5 * 24 * time.Hour)
	r.Check(ctx)
	time.Sleep(100 * time.Millisecond)
	if !kept() || len(poster.all()) != 1 {
		t.Fatalf("ten days on, sent five days ago: kept %v, %d posts", kept(), len(poster.all()))
	}
	// Not heard of for more than a week: forgotten.
	later(3 * 24 * time.Hour)
	r.Check(ctx)
	if kept() {
		t.Error("the mark of a firing long over is still kept")
	}
}
