package agentapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"homecloud/control-plane/internal/sandbox"
	"homecloud/control-plane/internal/sandbox/sandboxtest"
)

// withSandboxes gives s sandboxes on a fake Docker, whose fake Pi calls back
// at s's sandbox API.
func withSandboxes(t *testing.T, s *Server) *sandboxtest.Docker {
	t.Helper()
	d := sandboxtest.New(t)
	api := httptest.NewServer(s.SandboxHandler())
	t.Cleanup(api.Close)
	s.Sandboxes = &sandbox.Manager{Docker: &sandbox.Docker{Addr: d.Addr()}, Image: "registry/system/sandbox:1", Network: "sandboxes", API: api.URL, Memory: 1 << 30, CPUs: 1, Pids: 100}
	return d
}

func chatIn(t *testing.T, s *Server, chat, model string, msgs ...Message) (reasoning, content string) {
	t.Helper()
	w := post(t, s, "tok-alice", map[string]string{ChatHeader: chat}, map[string]any{"model": model, "stream": true, "messages": msgs})
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	reasoning, content, done := streamed(t, w.Body.String())
	if !done {
		t.Fatalf("not finished: %s", w.Body)
	}
	return
}

func TestAChatRunsInItsOwnSandbox(t *testing.T) {
	s, _, mcp := setup(t)
	d := withSandboxes(t, s)
	_, content := chatIn(t, s, "chat-1", "homepage/reader", Message{Role: "user", Content: "hello"})
	if content != "echo: hello" {
		t.Fatalf("content %q", content)
	}
	// The next message goes to the same sandbox, and only it: Pi keeps the
	// conversation.
	reasoning, content := chatIn(t, s, "chat-1", "homepage/reader",
		Message{Role: "user", Content: "hello"}, Message{Role: "assistant", Content: "echo: hello"},
		Message{Role: "user", Content: "which tools do you have?"})
	if content != "tools: 1" {
		t.Fatalf("content %q", content)
	}
	for _, want := range []string{"Let me look.", `list_repos({"namespace":"homepage"})`, "hmm"} {
		if !strings.Contains(reasoning, want) {
			t.Errorf("reasoning %q lacks %q", reasoning, want)
		}
	}
	if d.Created != 1 {
		t.Errorf("%d containers for one chat", d.Created)
	}
	_ = mcp
	// Another chat with the same agent gets its own sandbox.
	chatIn(t, s, "chat-2", "homepage/reader", Message{Role: "user", Content: "hi"})
	if d.Created != 2 || d.Running() != 2 {
		t.Errorf("created %d, running %d", d.Created, d.Running())
	}
	for _, c := range d.Containers {
		if c.Image != "registry/system/sandbox:1" || c.Labels[sandbox.Label] != "true" || c.Labels["homecloud.person"] != "alice" {
			t.Errorf("container %+v", c)
		}
		for _, e := range c.Env {
			if strings.Contains(e, "llm-key") || strings.Contains(e, "tok-alice") {
				t.Errorf("a sandbox holds a secret: %s", e)
			}
		}
	}
}

func TestTheSandboxActsOnlyDuringATurn(t *testing.T) {
	s, _, _ := setup(t)
	d := withSandboxes(t, s)
	chatIn(t, s, "chat-1", "homepage/reader", Message{Role: "user", Content: "hello"})
	var token string
	for _, c := range d.Containers {
		for _, e := range c.Env {
			if v, ok := strings.CutPrefix(e, "HOMECLOUD_TOKEN="); ok {
				token = v
			}
		}
	}
	h := s.SandboxHandler()
	for _, tok := range []string{token, "made-up", ""} {
		r := httptest.NewRequest("GET", "/tools", nil)
		r.Header.Set("Authorization", "Bearer "+tok)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("token %q between turns: %d", tok, w.Code)
		}
	}
}

func TestAChangedAgentRestartsItsSandbox(t *testing.T) {
	s, _, _ := setup(t)
	d := withSandboxes(t, s)
	chatIn(t, s, "chat-1", "homepage/reader", Message{Role: "user", Content: "hello"})
	list := s.Agents.(fakeAgents)
	list[0].Prompt = "You read files carefully."
	chatIn(t, s, "chat-1", "homepage/reader", Message{Role: "user", Content: "again"})
	if d.Created != 2 || d.Running() != 1 {
		t.Errorf("created %d, running %d", d.Created, d.Running())
	}
}

func TestWithoutAChatTheTurnRunsHere(t *testing.T) {
	s, llm, _ := setup(t, Message{Role: "assistant", Content: "in process"})
	d := withSandboxes(t, s)
	_, content, _ := streamed(t, post(t, s, "tok-alice", nil, map[string]any{"model": "homepage/reader", "stream": true, "messages": []Message{{Role: "user", Content: "hi"}}}).Body.String())
	if content != "in process" || d.Created != 0 || len(llm.requests) != 1 {
		t.Errorf("content %q, %d containers", content, d.Created)
	}
}

func TestPromptFrom(t *testing.T) {
	msgs := []Message{
		{Role: "system", Content: "Here's the thread history: bob: hi"},
		{Role: "user", Content: "first"},
		{Role: "assistant", Content: "answer"},
		{Role: "user", Content: []any{
			map[string]any{"type": "text", "text": "look"},
			map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,iVBOR"}},
		}},
	}
	text, images := promptFrom(msgs, false)
	if !strings.Contains(text, "thread history") || strings.Contains(text, "first") || !strings.HasSuffix(text, "look") {
		t.Errorf("text %q", text)
	}
	if len(images) != 1 || images[0].MimeType != "image/png" || images[0].Data != "iVBOR" {
		t.Errorf("images %+v", images)
	}
	text, _ = promptFrom(msgs, true)
	if !strings.Contains(text, "user: first\nassistant: answer") {
		t.Errorf("fresh text %q", text)
	}
}

func TestTheAnswerStreamsAsItIsWritten(t *testing.T) {
	s, _, _ := setup(t)
	withSandboxes(t, s)
	w := post(t, s, "tok-alice", map[string]string{ChatHeader: "chat-1"}, map[string]any{"model": "homepage/reader", "stream": true, "messages": []Message{{Role: "user", Content: "hello there"}}})
	chunks := 0
	for _, line := range strings.Split(w.Body.String(), "\n") {
		if strings.Contains(line, `"content":"`) && !strings.Contains(line, `"content":""`) {
			chunks++
		}
	}
	if _, content, _ := streamed(t, w.Body.String()); content != "echo: hello there" || chunks < 2 {
		t.Errorf("content %q in %d chunks; want it in pieces as Pi writes it", content, chunks)
	}
}
