package agentapi

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"garrison/control-plane/internal/agents"
	"garrison/control-plane/internal/gateway"
)

type fakeAgents []agents.Agent

func (f fakeAgents) All(context.Context) ([]agents.Agent, error) { return f, nil }
func (f fakeAgents) Get(_ context.Context, id string) (agents.Agent, bool, error) {
	for _, a := range f {
		if a.ID() == id {
			return a, true, nil
		}
	}
	return agents.Agent{}, false, nil
}

type fakeVerifier map[string]gateway.Person

func (f fakeVerifier) Verify(_ context.Context, tok string) (gateway.Person, error) {
	if p, ok := f[tok]; ok {
		return p, nil
	}
	return gateway.Person{}, errors.New("bad token")
}

// fakeLLM answers each request with the next of answers, and records them.
type fakeLLM struct {
	mu       sync.Mutex
	answers  []Message
	requests []map[string]any
}

func (f *fakeLLM) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer llm-key" {
		http.Error(w, "no key", 401)
		return
	}
	var body map[string]any
	json.NewDecoder(r.Body).Decode(&body)
	f.requests = append(f.requests, body)
	if body["stream"] == true { // a forwarded task
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"A title\"}}]}\n\ndata: [DONE]\n\n")
		return
	}
	a := Message{Role: "assistant", Content: "no more answers"}
	if len(f.answers) > 0 {
		a, f.answers = f.answers[0], f.answers[1:]
	}
	json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": a}}})
}

// fakeMCP is the gateway: it records who called which tool for which agent.
type fakeMCP struct {
	mu     sync.Mutex
	calls  []string // "token agent tool"
	seen   []string // "owner|started by" of each request
	merges []string // the merge mode a request named, if it named one
}

func (f *fakeMCP) mergeModes() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.merges)
}

func (f *fakeMCP) headers() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.seen...)
}

// sawUnattended tells whether a request came without a token, for owner,
// started by startedBy, with the service key.
func (f *fakeMCP) sawUnattended(owner, startedBy string) bool {
	for _, h := range f.headers() {
		if h == "|"+owner+"|"+startedBy+"|k" {
			return true
		}
	}
	return false
}

func (f *fakeMCP) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seen = append(f.seen, r.Header.Get("Authorization")+"|"+r.Header.Get(gateway.OwnerHeader)+"|"+r.Header.Get(gateway.StartedByHeader)+"|"+r.Header.Get(gateway.ServiceHeader))
	if v := r.Header.Get(gateway.MergeHeader); v != "" {
		f.merges = append(f.merges, v)
	}
	var req struct {
		Method string `json:"method"`
		Params struct {
			Name string `json:"name"`
		} `json:"params"`
	}
	json.NewDecoder(r.Body).Decode(&req)
	who := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ") + " " + r.Header.Get(AgentHeader)
	switch req.Method {
	case "tools/list":
		io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"read_file","description":"Reads a file.","inputSchema":{"type":"object"}}]}}`)
	case "tools/call":
		f.calls = append(f.calls, who+" "+req.Params.Name)
		if req.Params.Name != "read_file" {
			io.WriteString(w, `{"jsonrpc":"2.0","id":1,"error":{"code":-32602,"message":"unknown tool `+req.Params.Name+`"}}`)
			return
		}
		io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"services: {}"}]}}`)
	}
}

func toolCall(id, name, args string) ToolCall {
	var c ToolCall
	c.ID, c.Type = id, "function"
	c.Function.Name, c.Function.Arguments = name, args
	return c
}

func setup(t *testing.T, answers ...Message) (*Server, *fakeLLM, *fakeMCP) {
	t.Helper()
	llm := &fakeLLM{answers: answers}
	ls := httptest.NewServer(llm)
	t.Cleanup(ls.Close)
	mcp := &fakeMCP{}
	ms := httptest.NewServer(mcp)
	t.Cleanup(ms.Close)
	s := &Server{
		Agents: fakeAgents{
			{Unit: "homepage", Name: "reader", Description: "Reads", Tools: []string{"read_file"}, Prompt: "You read files."},
			{Unit: "garrison", Name: "operator", Model: "big-model", Prompt: "You operate."},
		},
		Verifier:     fakeVerifier{"tok-alice": {Name: "alice"}},
		LLM:          &LLM{URL: ls.URL, Key: "llm-key"},
		Gateway:      &Gateway{URL: ms.URL},
		Domain:       "example.com",
		DefaultModel: "small-model",
	}
	return s, llm, mcp
}

func post(t *testing.T, s *Server, token string, headers map[string]string, body any) *httptest.ResponseRecorder {
	t.Helper()
	b, _ := json.Marshal(body)
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(string(b)))
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w
}

// streamed reads a streamed answer: its reasoning and its content.
func streamed(t *testing.T, body string) (reasoning, content string, done bool) {
	t.Helper()
	sc := bufio.NewScanner(strings.NewReader(body))
	for sc.Scan() {
		line, ok := strings.CutPrefix(sc.Text(), "data: ")
		if !ok {
			continue
		}
		if line == "[DONE]" {
			done = true
			continue
		}
		var c struct {
			Choices []struct {
				Delta map[string]any `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(line), &c); err != nil {
			t.Fatalf("chunk %q: %v", line, err)
		}
		r, _ := c.Choices[0].Delta["reasoning_content"].(string)
		x, _ := c.Choices[0].Delta["content"].(string)
		reasoning += r
		content += x
	}
	return
}

func TestModelsAreTheAgents(t *testing.T) {
	s, _, _ := setup(t)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	// garrison's agents; the instances in units are not models.
	if w.Code != 200 || strings.Contains(w.Body.String(), `"id":"homepage/reader"`) || !strings.Contains(w.Body.String(), `"id":"garrison/operator"`) || !strings.Contains(w.Body.String(), `"name":"operator"`) {
		t.Errorf("models: %d %s", w.Code, w.Body)
	}
}

func TestAChatNeedsAToken(t *testing.T) {
	s, llm, _ := setup(t)
	for _, tok := range []string{"", "forged"} {
		if w := post(t, s, tok, nil, map[string]any{"model": "homepage/reader", "messages": []any{}}); w.Code != http.StatusUnauthorized {
			t.Errorf("token %q: %d", tok, w.Code)
		}
	}
	if len(llm.requests) != 0 {
		t.Error("the LLM was asked without a person")
	}
}

func TestATurnUsesTheAgentsToolsAsThePerson(t *testing.T) {
	s, llm, mcp := setup(t,
		Message{Role: "assistant", ToolCalls: []ToolCall{toolCall("c1", "read_file", `{"repo":"todo","path":"compose.yaml"}`), toolCall("c2", "propose_change", `{}`)}},
		Message{Role: "assistant", Content: "It has no services."},
	)
	w := post(t, s, "tok-alice", map[string]string{ChatHeader: "chat-1"}, map[string]any{
		"model": "homepage/reader", "stream": true,
		"messages": []any{
			map[string]any{"role": "system", "content": "Here's the thread history: bob: hi"},
			map[string]any{"role": "user", "content": "What is in todo's compose.yaml?"},
		},
	})
	reasoning, content, done := streamed(t, w.Body.String())
	if w.Code != 200 || content != "It has no services." || !done {
		t.Fatalf("answer: %d %q done=%v\n%s", w.Code, content, done, w.Body)
	}
	if !strings.Contains(reasoning, "read_file(") || !strings.Contains(reasoning, "unknown tool propose_change") {
		t.Errorf("reasoning = %q", reasoning)
	}
	// The tools went to the gateway with alice's token, for this agent; the
	// one it lacks was refused there.
	if strings.Join(mcp.calls, ",") != "tok-alice homepage/reader read_file,tok-alice homepage/reader propose_change" {
		t.Errorf("tool calls = %v", mcp.calls)
	}
	first := llm.requests[0]
	msgs := first["messages"].([]any)
	sys := msgs[0].(map[string]any)["content"].(string)
	if first["model"] != "small-model" || !strings.Contains(sys, `You are reader, an agent of Garrison at example.com, here working in the unit "homepage"`) || !strings.Contains(sys, "Rules of the platform") {
		t.Errorf("first request: model %v, system %q", first["model"], sys)
	}
	if msgs[1].(map[string]any)["content"] != "Here's the thread history: bob: hi" || msgs[2].(map[string]any)["role"] != "user" {
		t.Errorf("messages = %v", msgs)
	}
	if tools := first["tools"].([]any); len(tools) != 1 {
		t.Errorf("tools = %v", tools)
	}
	// The tool results went back to the LLM.
	second := llm.requests[1]["messages"].([]any)
	if last := second[len(second)-1].(map[string]any); last["role"] != "tool" || !strings.Contains(last["content"].(string), "unknown tool") {
		t.Errorf("last message of the second request = %v", last)
	}
	if !strings.Contains(second[len(second)-2].(map[string]any)["content"].(string), "services: {}") {
		t.Errorf("read_file's result missing: %v", second)
	}
}

func TestAnAgentsOwnModel(t *testing.T) {
	s, llm, _ := setup(t, Message{Role: "assistant", Content: "Fine."})
	w := post(t, s, "tok-alice", nil, map[string]any{"model": "garrison/operator", "messages": []any{map[string]any{"role": "user", "content": "Hi"}}})
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	json.Unmarshal(w.Body.Bytes(), &out)
	if w.Code != 200 || len(out.Choices) != 1 || out.Choices[0].Message.Content != "Fine." {
		t.Errorf("answer: %d %s", w.Code, w.Body)
	}
	if llm.requests[0]["model"] != "big-model" {
		t.Errorf("model = %v", llm.requests[0]["model"])
	}
}

func TestUnknownAgent(t *testing.T) {
	s, llm, _ := setup(t)
	if w := post(t, s, "tok-alice", nil, map[string]any{"model": "homepage/nobody", "messages": []any{}}); w.Code != http.StatusNotFound {
		t.Errorf("unknown agent: %d", w.Code)
	}
	if len(llm.requests) != 0 {
		t.Error("the LLM was asked")
	}
}

func TestTasksGoStraightToTheLLM(t *testing.T) {
	s, llm, mcp := setup(t)
	w := post(t, s, "tok-alice", map[string]string{TaskHeader: "title_generation"}, map[string]any{
		"model": "homepage/reader", "stream": true, "tools": []any{map[string]any{"type": "function"}},
		"messages": []any{map[string]any{"role": "user", "content": "Make a title"}},
	})
	if w.Code != 200 || !strings.Contains(w.Body.String(), "A title") {
		t.Errorf("task: %d %s", w.Code, w.Body)
	}
	if len(llm.requests) != 1 || llm.requests[0]["model"] != "small-model" || llm.requests[0]["tools"] != nil {
		t.Errorf("requests = %v", llm.requests)
	}
	if len(mcp.calls) != 0 {
		t.Error("a task used tools")
	}
}

func TestATurnEnds(t *testing.T) {
	loop := Message{Role: "assistant", ToolCalls: []ToolCall{toolCall("c", "read_file", `{}`)}}
	s, llm, _ := setup(t, loop, loop, loop, Message{Role: "assistant", Content: "Done looking."})
	s.MaxRounds = 2
	w := post(t, s, "tok-alice", nil, map[string]any{"model": "homepage/reader", "messages": []any{map[string]any{"role": "user", "content": "Look forever"}}})
	if !strings.Contains(w.Body.String(), "Done looking.") && !strings.Contains(w.Body.String(), "read_file") {
		t.Errorf("answer: %s", w.Body)
	}
	if len(llm.requests) != 3 || llm.requests[2]["tools"] != nil {
		t.Errorf("%d requests, the last with tools %v", len(llm.requests), llm.requests[len(llm.requests)-1]["tools"])
	}
}

// A model is garrison's agent; in a unit's channel, it works there.
func TestAChatRunsTheAgentWhereItsChannelIs(t *testing.T) {
	s := &Server{Poster: &fakePoster{channels: map[string]gateway.ChannelInfo{
		"c-lab": {Unit: "lab", Members: true}, "c-garrison": {Unit: "garrison"},
	}}}
	for _, c := range []struct{ model, chat, want string }{
		{"garrison/operator", "channel:c-lab", "lab/operator"},
		{"operator", "channel:c-lab", "lab/operator"},
		{"garrison/operator", "channel:c-garrison", "garrison/operator"},
		{"garrison/operator", "chat-1", "garrison/operator"}, // a private chat: in no unit
		{"todo/operator", "channel:c-lab", "todo/operator"},  // an instance named as it is
	} {
		if got, err := s.placed(context.Background(), c.model, c.chat); err != nil || got != c.want {
			t.Errorf("%s in %s: %q %v, want %q", c.model, c.chat, got, err, c.want)
		}
	}
}

// The dashboard asks which agents work now, with the service key; nobody
// else learns who works for whom.
func TestRunningNeedsTheServiceKey(t *testing.T) {
	s, _, _ := setup(t)
	s.Gateway = &Gateway{ServiceKey: "k"}
	for _, key := range []string{"", "wrong"} {
		r := httptest.NewRequest(http.MethodGet, "/internal/running", nil)
		r.Header.Set(gateway.ServiceHeader, key)
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		if w.Code != http.StatusForbidden {
			t.Errorf("key %q: %d", key, w.Code)
		}
	}
	r := httptest.NewRequest(http.MethodGet, "/internal/running", nil)
	r.Header.Set(gateway.ServiceHeader, "k")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != 200 || strings.TrimSpace(w.Body.String()) != "[]" {
		t.Errorf("without sandboxes: %d %s", w.Code, w.Body)
	}
}
