// Package agentapi is the agent service: an OpenAI-compatible API that
// Open WebUI talks to as if it were an LLM. Each model is an agent of a
// namespace, <namespace>/<agent>, read from its config repo; a chat with it
// runs the agent's loop here: the LLM with the agent's prompt and its tools
// from the gateway, which act as the person chatting. Open WebUI sends that
// person's Keycloak token, and holds no LLM key.
//
// With sandboxes, each chat with an agent runs in a container of its own:
// Pi, the agent harness, with a workspace kept between turns (sandbox.go).
// Without, or for a request without a chat, the loop runs in this process.
package agentapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"garrison/control-plane/internal/agents"
	"garrison/control-plane/internal/events"
	"garrison/control-plane/internal/gateway"
	"garrison/control-plane/internal/sandbox"
)

// Agents finds the agents defined in the namespaces' config repos.
type Agents interface {
	All(ctx context.Context) ([]agents.Agent, error)
	Get(ctx context.Context, id string) (agents.Agent, bool, error)
}

// Server serves /v1/models and /v1/chat/completions.
type Server struct {
	Agents       Agents
	Verifier     gateway.Verifier
	LLM          *LLM    // the LLM when Models is nil
	Models       *Models // the providers and models of system's config; nil: LLM and DefaultModel
	Gateway      *Gateway
	Domain       string
	DefaultModel string // the LLM model of agents that name none, and of Open WebUI's own tasks
	MaxRounds    int    // tool rounds per turn; 0: 25
	TurnTimeout  time.Duration
	Sandboxes    *sandbox.Manager // nil: every turn runs in this process
	MaxToolCalls int              // tool calls per turn in a sandbox; 0: 60
	Git          *GitProxy        // Git for the sandboxes; nil: none
	Gitea        *gateway.Gitea   // who may use an agent (visible); nil: everyone
	Poster       Poster           // posts answers no message waits for; nil: none
	Events       events.Recorder  // the feed, which gets the routines' runs; nil: none
	Questions    *Questions       // what routines asked people (ask_person); nil: they cannot ask
}

// Headers Open WebUI's connection fills in for each request.
const (
	ChatHeader = "X-Garrison-Chat" // {{CHAT_ID}}
	TaskHeader = "X-Garrison-Task" // {{TASK}}: title, tags, follow-ups...
)

// route is the LLM, and the model's name there, for what an agent names as
// its model ("" the default), and the model's full id.
func (s *Server) route(ctx context.Context, agent, model string) (*LLM, string, string) {
	if s.Models != nil {
		return s.Models.Route(ctx, agent, model)
	}
	if model == "" {
		model = s.DefaultModel
	}
	return s.LLM, model, model
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/models", s.models)
	mux.HandleFunc("POST /v1/chat/completions", s.chat)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	return mux
}

// models lists every agent. Everyone signed in may see every agent, and
// Open WebUI may ask without a person, so this needs no token.
func (s *Server) models(w http.ResponseWriter, r *http.Request) {
	list, err := s.Agents.All(r.Context())
	if err != nil {
		log.Printf("agents: models: %v", err)
		http.Error(w, "cannot list the agents", http.StatusBadGateway)
		return
	}
	data := make([]map[string]any, 0, len(list))
	for _, a := range list {
		data = append(data, map[string]any{"id": a.ID(), "object": "model", "created": 0, "owned_by": "garrison", "name": a.ID()})
	}
	writeJSON(w, map[string]any{"object": "list", "data": data})
}

type chatRequest struct {
	Model    string    `json:"model"`
	Messages []Message `json:"messages"`
	Stream   bool      `json:"stream"`
}

func (s *Server) chat(w http.ResponseWriter, r *http.Request) {
	raw, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || raw == "" {
		apiError(w, http.StatusUnauthorized, "a Keycloak access token is required")
		return
	}
	person, err := s.Verifier.Verify(r.Context(), raw)
	if err != nil {
		log.Printf("agents: refused token: %v", err)
		apiError(w, http.StatusUnauthorized, "invalid token")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 32<<20))
	if err != nil {
		apiError(w, http.StatusBadRequest, "cannot read the request")
		return
	}
	var req chatRequest
	if err := json.Unmarshal(body, &req); err != nil || req.Model == "" {
		apiError(w, http.StatusBadRequest, "not a chat completion request")
		return
	}
	// Open WebUI's own tasks (a chat's title, tags, follow-up suggestions)
	// need no agent: they go straight to the LLM.
	if task := r.Header.Get(TaskHeader); task != "" {
		var generic map[string]any
		json.Unmarshal(body, &generic)
		llm, model, _ := s.route(r.Context(), "", "")
		if err := llm.Forward(r.Context(), w, generic, model); err != nil {
			log.Printf("agents: %s: task %s: %v", person.Name, task, err)
			apiError(w, http.StatusBadGateway, err.Error())
		}
		return
	}
	agent, found, err := s.Agents.Get(r.Context(), req.Model)
	if err != nil {
		log.Printf("agents: %s: %s: %v", person.Name, req.Model, err)
		apiError(w, http.StatusBadGateway, "cannot read the agent")
		return
	}
	if !found {
		apiError(w, http.StatusNotFound, "no agent "+req.Model)
		return
	}
	if s.Gitea != nil {
		ok, err := s.Gitea.CanUse(r.Context(), &agent, person)
		if err != nil {
			apiError(w, http.StatusBadGateway, "cannot read the namespace's members")
			return
		}
		if !ok {
			log.Printf("agents: %s: %s: refused: not in %s", person.Name, agent.ID(), agent.Namespace)
			apiError(w, http.StatusForbidden, fmt.Sprintf("%s is for the people of %s; ask one of its admins to add you to members.yaml, or ask an agent that serves everyone", agent.ID(), agent.Namespace))
			return
		}
	}
	chat := r.Header.Get(ChatHeader)
	if why, err := s.privateHere(r.Context(), agent, chat); err != nil {
		apiError(w, http.StatusBadGateway, err.Error())
		return
	} else if why != "" {
		log.Printf("agents: %s: %s: refused in %s: private", person.Name, agent.ID(), chat)
		apiError(w, http.StatusForbidden, why)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.turnTimeout())
	defer cancel()

	out := newWriter(w, req.Model, req.Stream)
	var rounds int
	if s.Sandboxes != nil && chat != "" {
		run := sandbox.Run{Token: raw, StartedBy: "person", Chain: []string{agent.ID()}, Admin: person.Admin, Show: out.reasoning}
		msg := func(fresh bool) (string, []sandbox.Image) { return promptFrom(req.Messages, fresh) }
		rounds, err = s.sandboxTurn(ctx, run, person.Name, chat, chat+"\x00"+agent.ID(), agent, msg, out)
	} else {
		rounds, err = s.turn(ctx, raw, agent, req.Messages, out)
	}
	if err != nil {
		log.Printf("agents: %s: %s: chat %s: %v", person.Name, agent.ID(), chat, err)
		out.content("\n\nThe agent stopped: " + err.Error())
	}
	log.Printf("agents: %s: %s: chat %s: %d tool calls", person.Name, agent.ID(), chat, rounds)
	out.finish()
}

func (s *Server) turnTimeout() time.Duration {
	if s.TurnTimeout > 0 {
		return s.TurnTimeout
	}
	return 10 * time.Minute
}

// turn runs one turn of the agent: the LLM answers, calling the agent's
// tools as the person, until it answers without a tool call. It returns
// the number of tool calls.
func (s *Server) turn(ctx context.Context, token string, a agents.Agent, incoming []Message, out *writer) (int, error) {
	a.Internet = agents.Internet{} // without a sandbox, there is none
	messages := []Message{{Role: "system", Content: agents.System(a, s.Domain)}}
	for _, m := range incoming {
		switch m.Role {
		case "system", "user", "assistant":
			// Open WebUI's own system messages, e.g. a channel thread's history,
			// come after the agent's prompt and the platform's rules.
			messages = append(messages, Message{Role: m.Role, Content: m.Content})
		}
	}
	as := As{Token: token, StartedBy: "person"}
	tools, err := s.Gateway.Tools(ctx, as, a.ID())
	if err != nil {
		return 0, err
	}
	llm, model, _ := s.route(ctx, a.ID(), a.Model)
	max := s.MaxRounds
	if max <= 0 {
		max = 25
	}
	calls := 0
	for round := 0; ; round++ {
		offer := tools
		if round == max {
			offer = nil // the last round must answer
			messages = append(messages, Message{Role: "system", Content: "You have used all your tool calls for this turn. Answer now with what you found."})
		}
		msg, err := llm.Chat(ctx, model, messages, offer)
		if err != nil {
			return calls, err
		}
		if len(msg.ToolCalls) == 0 || offer == nil {
			out.content(msg.Text())
			return calls, nil
		}
		messages = append(messages, Message{Role: "assistant", Content: msg.Text(), ToolCalls: msg.ToolCalls})
		for _, c := range msg.ToolCalls {
			calls++
			out.reasoning(fmt.Sprintf("%s(%s)\n", c.Function.Name, shorten(c.Function.Arguments, 200)))
			text, isErr, err := s.Gateway.Call(ctx, as, a.ID(), c.Function.Name, c.Function.Arguments)
			if err != nil {
				if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
					return calls, err
				}
				text, isErr = "error: "+err.Error(), true
			}
			if isErr {
				out.reasoning("  → " + shorten(text, 300) + "\n")
			}
			messages = append(messages, Message{Role: "tool", ToolCallID: c.ID, Content: text})
		}
	}
}

func shorten(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// writer answers Open WebUI: streamed as chunks, or as one completion.
// Tool calls show as reasoning, the answer as content.
type writer struct {
	mu      sync.Mutex // agents asked in the run write from other goroutines
	w       http.ResponseWriter
	model   string
	stream  bool
	id      string
	created int64
	text    strings.Builder
	thought strings.Builder
}

func newWriter(w http.ResponseWriter, model string, stream bool) *writer {
	out := &writer{w: w, model: model, stream: stream, id: fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano()), created: time.Now().Unix()}
	if stream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		out.chunk(map[string]any{"role": "assistant", "content": ""}, nil)
	}
	return out
}

func (o *writer) chunk(delta map[string]any, finish any) {
	b, _ := json.Marshal(map[string]any{
		"id": o.id, "object": "chat.completion.chunk", "created": o.created, "model": o.model,
		"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}},
	})
	fmt.Fprintf(o.w, "data: %s\n\n", b)
	if f, ok := o.w.(http.Flusher); ok {
		f.Flush()
	}
}

func (o *writer) reasoning(s string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.stream {
		o.chunk(map[string]any{"reasoning_content": s}, nil)
	} else {
		o.thought.WriteString(s)
	}
}

func (o *writer) content(s string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.stream {
		o.chunk(map[string]any{"content": s}, nil)
	} else {
		o.text.WriteString(s)
	}
}

func (o *writer) finish() {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.stream {
		o.chunk(map[string]any{}, "stop")
		fmt.Fprint(o.w, "data: [DONE]\n\n")
		if f, ok := o.w.(http.Flusher); ok {
			f.Flush()
		}
		return
	}
	msg := map[string]any{"role": "assistant", "content": o.text.String()}
	if o.thought.Len() > 0 {
		msg["reasoning_content"] = o.thought.String()
	}
	writeJSON(o.w, map[string]any{
		"id": o.id, "object": "chat.completion", "created": o.created, "model": o.model,
		"choices": []any{map[string]any{"index": 0, "message": msg, "finish_reason": "stop"}},
	})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func apiError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": msg, "type": "garrison"}})
}
