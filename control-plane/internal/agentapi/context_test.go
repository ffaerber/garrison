package agentapi

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestTheContextWindowComesFromTheServer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/llamacpp/v1/models":
			io.WriteString(w, `{"data": [{"id": "other", "meta": {"n_ctx": 8192}}, {"id": "qwen3.8-27b", "meta": {"n_ctx": 65536}}]}`)
		case "/vllm/v1/models":
			io.WriteString(w, `{"data": [{"id": "big", "max_model_len": 131072}]}`)
		case "/ollama/v1/models":
			io.WriteString(w, `{"data": [{"id": "qwen3.8-64k:latest"}, {"id": "plain"}]}`)
		case "/ollama/api/show":
			b, _ := io.ReadAll(r.Body)
			if string(b) == `{"model":"qwen3.8-64k:latest"}` {
				io.WriteString(w, `{"parameters": "temperature 0.7\nnum_ctx 65536\n", "model_info": {"qwen35.context_length": 262144}}`)
			} else {
				io.WriteString(w, `{"parameters": "", "model_info": {"llama.context_length": 32768}}`)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	for _, c := range []struct {
		url, model string
		want       int
	}{
		{srv.URL + "/llamacpp/v1", "qwen3.8-27b", 65536},      // a llama.cpp server: meta.n_ctx
		{srv.URL + "/vllm/v1", "big", 131072},                 // vLLM: max_model_len
		{srv.URL + "/ollama/v1", "qwen3.8-64k:latest", 65536}, // Ollama: its num_ctx
		{srv.URL + "/ollama/v1", "plain", 32768},              // Ollama: the model's length
		{srv.URL + "/nothing/v1", "x", DefaultContext},        // says nothing
		{"http://127.0.0.1:1/v1", "unreachable", DefaultContext},
	} {
		if got := (&LLM{URL: c.url}).Context(context.Background(), c.model); got != c.want {
			t.Errorf("%s %s: %d, want %d", c.url, c.model, got, c.want)
		}
	}
	var none *LLM
	if none.Context(context.Background(), "x") != DefaultContext {
		t.Error("no LLM")
	}
}
