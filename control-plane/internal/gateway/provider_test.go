package gateway

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// add_provider proposes providers.yaml with the provider, as the root
// admin asking, and sends the key to the Models page, never the chat.
func TestAddProvider(t *testing.T) {
	var proposed, by, prBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v1/users/root":
			io.WriteString(w, `{"is_admin":true}`)
		case r.URL.Path == "/api/v1/users/alice":
			io.WriteString(w, `{"is_admin":false}`)
		case r.URL.Path == "/api/v1/repos/garrison/config/raw/llm/providers.yaml":
			io.WriteString(w, "# the providers\ndefault: ollama-cloud/gpt-oss:120b\nproviders:\n  ollama-cloud:\n    url: https://ollama.com/v1\n")
		case r.URL.Path == "/api/v1/repos/garrison/config/contents/llm/providers.yaml" && r.Method == http.MethodGet:
			io.WriteString(w, `{"sha":"p1"}`)
		case r.URL.Path == "/api/v1/repos/garrison/config/contents/llm/providers.yaml":
			by = r.Header.Get("Sudo")
			var body struct {
				Content string `json:"content"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			data, _ := base64.StdEncoding.DecodeString(body.Content)
			proposed = string(data)
			io.WriteString(w, "{}")
		case r.URL.Path == "/api/v1/repos/garrison/config/pulls":
			var body struct {
				Body string `json:"body"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			prBody = body.Body
			io.WriteString(w, `{"number":7,"html_url":"https://git/garrison/config/pulls/7"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	tools := &Tools{Gitea: &Gitea{URL: srv.URL, User: "admin", Password: "pw"}, Dashboard: "https://home.test"}
	call := func(who string, args map[string]any) (string, error) {
		raw, _ := json.Marshal(args)
		return tools.Call(context.Background(), Person{Name: who}, nil, "add_provider", raw)
	}

	out, err := call("root", map[string]any{"name": "openrouter", "url": "https://openrouter.ai/api/v1", "include": []string{"anthropic/*"}})
	if err != nil {
		t.Fatal(err)
	}
	if by != "root" || !strings.Contains(proposed, "# the providers") || !strings.Contains(proposed, "openrouter:\n    url: https://openrouter.ai/api/v1") ||
		!strings.Contains(proposed, `include: ["anthropic/*"]`) {
		t.Errorf("by %s:\n%s", by, proposed)
	}
	for _, want := range []string{"https://home.test/models?provider=openrouter", "https://home.test/c/garrison/config/7", "never into this chat"} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q in %s", want, out)
		}
	}
	if !strings.Contains(prBody, "Models page") {
		t.Errorf("pull request: %s", prBody)
	}

	// Only root admins; a bad name or URL, or one there already, is refused.
	if _, err := call("alice", map[string]any{"name": "groq", "url": "https://api.groq.com/openai/v1"}); !errors.Is(err, ErrForbidden) {
		t.Errorf("alice: %v", err)
	}
	for _, args := range []map[string]any{
		{"name": "ollama-cloud", "url": "https://ollama.com/v1"},
		{"name": "Groq", "url": "https://api.groq.com/openai/v1"},
		{"name": "groq", "url": "https://api.groq.com/openai/v1/chat/completions"},
	} {
		if _, err := call("root", args); !errors.Is(err, ErrInvalid) {
			t.Errorf("%v: %v", args, err)
		}
	}
	// The tool takes no key.
	for _, s := range baseSpecs() {
		if s.Name == "add_provider" && strings.Contains(string(mustJSON(s)), `"key"`) {
			t.Error("add_provider takes a key")
		}
	}
}

func mustJSON(v any) []byte { b, _ := json.Marshal(v); return b }
