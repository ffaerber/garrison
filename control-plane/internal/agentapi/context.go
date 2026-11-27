package agentapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// A model's context window: how many tokens a request may hold, prompt and
// answer together. Pi compacts a conversation before it gets that long, and
// asks for an answer that fits; told too much, it sends requests the server
// refuses ("exceeds the context window"), as a long channel conversation
// did on a server with 64k.

// DefaultContext is the window of a model whose server does not say.
const DefaultContext = 128000

// contextCache keeps what servers said, per URL and model, for a while.
var contextCache = struct {
	sync.Mutex
	m map[string]contextEntry
}{m: map[string]contextEntry{}}

type contextEntry struct {
	n  int
	at time.Time
}

const contextFor = 10 * time.Minute

// Context is model's context window on l's server, or DefaultContext if it
// does not say. It asks the OpenAI-style /models, whose entries some
// servers give the window (llama.cpp's meta.n_ctx, vLLM's max_model_len,
// others' context_length), then Ollama's /api/show (num_ctx, else the
// model's own context length).
func (l *LLM) Context(ctx context.Context, model string) int {
	if l == nil || l.URL == "" || model == "" {
		return DefaultContext
	}
	k := l.URL + "\x00" + model
	contextCache.Lock()
	if e, ok := contextCache.m[k]; ok && time.Since(e.at) < contextFor {
		contextCache.Unlock()
		return e.n
	}
	contextCache.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	n := l.fromModels(ctx, model)
	if n == 0 {
		n = l.fromOllama(ctx, model)
	}
	if n < 2048 {
		n = DefaultContext
	}
	contextCache.Lock()
	contextCache.m[k] = contextEntry{n: n, at: time.Now()}
	contextCache.Unlock()
	return n
}

func (l *LLM) get(ctx context.Context, method, url string, body []byte, out any) bool {
	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
	if err != nil {
		return false
	}
	if k := l.key(); k != "" {
		req.Header.Set("Authorization", "Bearer "+k)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := l.client().Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK && json.NewDecoder(resp.Body).Decode(out) == nil
}

func (l *LLM) fromModels(ctx context.Context, model string) int {
	var list struct {
		Data []map[string]any `json:"data"`
	}
	if !l.get(ctx, "GET", strings.TrimSuffix(l.URL, "/")+"/models", nil, &list) {
		return 0
	}
	for _, m := range list.Data {
		if m["id"] != model {
			continue
		}
		for _, k := range []string{"context_length", "max_model_len", "context_window", "max_context_length"} {
			if n := number(m[k]); n > 0 {
				return n
			}
		}
		if meta, ok := m["meta"].(map[string]any); ok {
			return number(meta["n_ctx"])
		}
	}
	return 0
}

var numCtx = regexp.MustCompile(`(?m)^num_ctx\s+(\d+)`)

func (l *LLM) fromOllama(ctx context.Context, model string) int {
	base := strings.TrimSuffix(strings.TrimSuffix(l.URL, "/"), "/v1")
	body, _ := json.Marshal(map[string]string{"model": model})
	var show struct {
		Parameters string         `json:"parameters"`
		ModelInfo  map[string]any `json:"model_info"`
	}
	if !l.get(ctx, "POST", base+"/api/show", body, &show) {
		return 0
	}
	if m := numCtx.FindStringSubmatch(show.Parameters); m != nil {
		n, _ := strconv.Atoi(m[1])
		return n
	}
	for k, v := range show.ModelInfo {
		if strings.HasSuffix(k, ".context_length") {
			// Ollama runs a model with its default context unless num_ctx
			// says more; the model's own length is the most it could be.
			return number(v)
		}
	}
	return 0
}

func number(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case string:
		i, _ := strconv.Atoi(n)
		return i
	}
	return 0
}
