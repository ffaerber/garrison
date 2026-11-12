package agentapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"

	"homecloud/control-plane/internal/agents"
	"homecloud/control-plane/internal/sandbox"
)

// Workspace is added to an agent's prompt when it runs in a sandbox.
const Workspace = `## Your sandbox

You run in a sandbox of your own for this conversation: a Linux container with bash, git, ripgrep, curl and jq. Your working directory, /data/work, is kept between messages, and only this conversation sees it. Use read, write, edit and bash there for drafts, scripts and checks.

The sandbox has no internet access and cannot reach the platform's services directly: use your platform tools for repos, apps and changes; they act as the person you talk to. Files in your workspace reach a repo only through a proposed change.`

// definition changes whenever what the sandbox was started with does.
func definition(model, prompt string, a agents.Agent) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s\x00%s\x00%s", model, prompt, strings.Join(a.Tools, ","))
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// sandboxTurn runs the turn in the chat's sandbox: Pi with the agent's
// prompt, its own tools in its workspace and the agent's platform tools.
func (s *Server) sandboxTurn(ctx context.Context, token, person, chat string, a agents.Agent, incoming []Message, out *writer) (int, error) {
	model := a.Model
	if model == "" {
		model = s.DefaultModel
	}
	prompt := agents.System(a, s.Domain) + "\n\n" + Workspace
	want := sandbox.Want{
		Key: chat + "\x00" + a.ID(), Chat: chat, Agent: a.ID(), Person: person,
		Model: model, Prompt: prompt, Definition: definition(model, prompt, a),
	}
	max := s.MaxToolCalls
	if max <= 0 {
		max = 60
	}
	calls := 0
	err := s.Sandboxes.Turn(ctx, want, token, func(sb *sandbox.Session) error {
		pi := sb.Pi()
		st, err := pi.State(ctx)
		if err != nil {
			return err
		}
		text, images := promptFrom(incoming, st.MessageCount == 0)
		turn, cancel := context.WithCancel(ctx)
		defer cancel()
		capped := false
		var failure string
		err = pi.Prompt(turn, text, images, func(ev sandbox.Event) {
			switch ev.Type {
			case "message_update":
				if ev.Delta != nil && ev.Delta.Type == "thinking_delta" {
					out.reasoning(ev.Delta.Delta)
				}
			case "message_end":
				m := ev.Message
				if m == nil || m.Role != "assistant" {
					return
				}
				switch m.StopReason {
				case "toolUse":
					if t := strings.TrimSpace(m.Text()); t != "" {
						out.reasoning(t + "\n")
					}
				case "error":
					failure = m.ErrorMessage
				case "aborted":
				default:
					out.content(m.Text())
				}
			case "tool_execution_start":
				calls++
				out.reasoning(fmt.Sprintf("%s(%s)\n", ev.ToolName, shorten(string(ev.Args), 200)))
				if calls >= max && !capped {
					capped = true
					cancel()
				}
			case "tool_execution_end":
				if ev.IsError && ev.Result != nil {
					var b strings.Builder
					for _, c := range ev.Result.Content {
						b.WriteString(c.Text)
					}
					out.reasoning("  → " + shorten(b.String(), 300) + "\n")
				}
			case "auto_retry_start":
				out.reasoning("The LLM failed (" + shorten(ev.ErrorMessage, 200) + "); retrying.\n")
			case "extension_error":
				log.Printf("agents: %s: chat %s: extension error: %s", a.ID(), chat, ev.Error)
			}
		})
		if capped && ctx.Err() == nil {
			return fmt.Errorf("it used all %d tool calls of a turn; ask it to go on", max)
		}
		if err == nil && failure != "" {
			err = errors.New(failure)
		}
		return err
	})
	return calls, err
}

// promptFrom is what Pi gets of Open WebUI's request: the newest message
// with its images, and Open WebUI's own system messages (a channel thread's
// history). Pi keeps the conversation itself; only a fresh session, e.g.
// of a chat begun before sandboxes, gets the earlier messages as context.
func promptFrom(msgs []Message, fresh bool) (string, []sandbox.Image) {
	last := -1
	for i, m := range msgs {
		if m.Role == "user" {
			last = i
		}
	}
	var b strings.Builder
	for _, m := range msgs {
		if m.Role == "system" && strings.TrimSpace(m.Text()) != "" {
			b.WriteString("Context from the chat:\n" + m.Text() + "\n\n")
		}
	}
	if fresh && last > 0 {
		var earlier strings.Builder
		for _, m := range msgs[:last] {
			if (m.Role == "user" || m.Role == "assistant") && strings.TrimSpace(m.Text()) != "" {
				fmt.Fprintf(&earlier, "%s: %s\n", m.Role, m.Text())
			}
		}
		if earlier.Len() > 0 {
			b.WriteString("Earlier in this conversation, before your sandbox started:\n" + earlier.String() + "\n")
		}
	}
	var images []sandbox.Image
	if last >= 0 {
		b.WriteString(msgs[last].Text())
		if parts, ok := msgs[last].Content.([]any); ok {
			for _, p := range parts {
				part, _ := p.(map[string]any)
				if part["type"] != "image_url" {
					continue
				}
				iu, _ := part["image_url"].(map[string]any)
				u, _ := iu["url"].(string)
				// data:image/png;base64,...
				if head, data, ok := strings.Cut(strings.TrimPrefix(u, "data:"), ";base64,"); ok && strings.HasPrefix(u, "data:") {
					images = append(images, sandbox.Image{Type: "image", Data: data, MimeType: head})
				}
			}
		}
	}
	return b.String(), images
}

// SandboxHandler serves the sandboxes: the LLM, with the key added and the
// agent's model enforced, and the agent's platform tools, run at the gateway
// as the person whose turn runs. A sandbox names itself by the token it was
// started with, which counts only during a turn.
func (s *Server) SandboxHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", s.sandboxLLM)
	mux.HandleFunc("GET /tools", s.sandboxTools)
	mux.HandleFunc("POST /tools/call", s.sandboxCall)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	return mux
}

func (s *Server) caller(w http.ResponseWriter, r *http.Request) (agent, model, token string, ok bool) {
	raw, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	agent, model, token, ok = s.Sandboxes.Caller(raw)
	if !ok {
		apiError(w, http.StatusUnauthorized, "not a sandbox in a turn")
	}
	return
}

func (s *Server) sandboxLLM(w http.ResponseWriter, r *http.Request) {
	_, model, _, ok := s.caller(w, r)
	if !ok {
		return
	}
	var body map[string]any
	if err := json.NewDecoder(io.LimitReader(r.Body, 32<<20)).Decode(&body); err != nil {
		apiError(w, http.StatusBadRequest, "not a chat completion request")
		return
	}
	body["model"] = model
	if err := s.LLM.Proxy(r.Context(), w, body); err != nil {
		apiError(w, http.StatusBadGateway, err.Error())
	}
}

type toolSpec struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"input_schema"`
}

func (s *Server) sandboxTools(w http.ResponseWriter, r *http.Request) {
	agent, _, token, ok := s.caller(w, r)
	if !ok {
		return
	}
	tools, err := s.Gateway.Tools(r.Context(), token, agent)
	if err != nil {
		apiError(w, http.StatusBadGateway, err.Error())
		return
	}
	specs := make([]toolSpec, 0, len(tools))
	for _, t := range tools {
		specs = append(specs, toolSpec{t.Function.Name, t.Function.Description, t.Function.Parameters})
	}
	writeJSON(w, specs)
}

func (s *Server) sandboxCall(w http.ResponseWriter, r *http.Request) {
	agent, _, token, ok := s.caller(w, r)
	if !ok {
		return
	}
	var req struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 16<<20)).Decode(&req); err != nil || req.Name == "" {
		apiError(w, http.StatusBadRequest, "not a tool call")
		return
	}
	text, isErr, err := s.Gateway.Call(r.Context(), token, agent, req.Name, string(req.Arguments))
	if err != nil {
		text, isErr = "error: "+err.Error(), true
	}
	writeJSON(w, map[string]any{"text": text, "is_error": isErr})
}
