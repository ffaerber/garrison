package models

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A provider like Anthropic's OpenAI-compatible API lists its models only
// with its key and the anthropic-version header.
func TestListSendsWhatAnthropicWants(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("anthropic-version") == "" {
			http.Error(w, `{"error":{"message":"anthropic-version: header is required"}}`, http.StatusBadRequest)
			return
		}
		if r.Header.Get("Authorization") != "Bearer sk-ant" {
			http.Error(w, "no key", http.StatusUnauthorized)
			return
		}
		io.WriteString(w, `{"data":[{"id":"claude-sonnet-5-5","max_input_tokens":1000000},{"id":"claude-opus-5-5"}],"has_more":false}`)
	}))
	defer srv.Close()
	ids, err := List(context.Background(), srv.Client(), srv.URL+"/v1", "sk-ant")
	if err != nil || strings.Join(ids, " ") != "claude-opus-5-5 claude-sonnet-5-5" {
		t.Errorf("%v %v", ids, err)
	}
	if _, err := List(context.Background(), srv.Client(), srv.URL+"/v1", "wrong"); err == nil {
		t.Error("a wrong key listed the models")
	}
}
