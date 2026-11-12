package agentapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// LLM is an OpenAI-compatible chat completions API. Only the agent service
// holds its key; Open WebUI and the agents' sandboxes never see it.
type LLM struct {
	URL     string // e.g. https://ollama.com/v1
	Key     string
	KeyFile string // read on each request when Key is empty: it may be filled in by hand
	HTTP    *http.Client
}

func (l *LLM) key() string {
	if l.Key != "" || l.KeyFile == "" {
		return l.Key
	}
	b, _ := os.ReadFile(l.KeyFile)
	return strings.TrimSpace(string(b))
}

// Message is one chat message, as the OpenAI API has it.
type Message struct {
	Role       string     `json:"role"`
	Content    any        `json:"content"` // a string, or parts (text, images)
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

// Text is a message's text, also of a message in parts.
func (m Message) Text() string {
	switch c := m.Content.(type) {
	case string:
		return c
	case []any:
		var b strings.Builder
		for _, p := range c {
			if part, ok := p.(map[string]any); ok && part["type"] == "text" {
				s, _ := part["text"].(string)
				b.WriteString(s)
			}
		}
		return b.String()
	}
	return ""
}

type ToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// Tool is a function the model may call.
type Tool struct {
	Type     string `json:"type"` // function
	Function struct {
		Name        string         `json:"name"`
		Description string         `json:"description"`
		Parameters  map[string]any `json:"parameters"`
	} `json:"function"`
}

func (l *LLM) client() *http.Client {
	if l.HTTP != nil {
		return l.HTTP
	}
	return &http.Client{Timeout: 5 * time.Minute}
}

func (l *LLM) post(ctx context.Context, body any) (*http.Response, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(l.URL, "/")+"/chat/completions", bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if key := l.key(); key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := l.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("llm: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 2000))
		return nil, fmt.Errorf("llm: %s: %s", resp.Status, strings.TrimSpace(string(data)))
	}
	return resp, nil
}

// Chat asks for one answer, which may call tools.
func (l *LLM) Chat(ctx context.Context, model string, messages []Message, tools []Tool) (Message, error) {
	body := map[string]any{"model": model, "messages": messages, "stream": false}
	if len(tools) > 0 {
		body["tools"] = tools
	}
	resp, err := l.post(ctx, body)
	if err != nil {
		return Message{}, err
	}
	defer resp.Body.Close()
	var out struct {
		Choices []struct {
			Message Message `json:"message"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&out); err != nil {
		return Message{}, fmt.Errorf("llm: %w", err)
	}
	if len(out.Choices) == 0 {
		return Message{}, fmt.Errorf("llm: no answer")
	}
	return out.Choices[0].Message, nil
}

// Forward sends Open WebUI's own request (a title, tags, follow-ups) to the
// LLM on model and copies the answer back as it comes.
func (l *LLM) Forward(ctx context.Context, w http.ResponseWriter, body map[string]any, model string) error {
	body["model"] = model
	delete(body, "tools")
	return l.Proxy(ctx, w, body)
}

// Proxy sends a request as it is, with the key, and copies the answer back
// as it comes.
func (l *LLM) Proxy(ctx context.Context, w http.ResponseWriter, body map[string]any) error {
	resp, err := l.post(ctx, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 16<<10)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			w.Write(buf[:n])
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}
