package hooks

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func sign(secret, body string) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(body))
	return hex.EncodeToString(m.Sum(nil))
}

func TestHandlerWakesOnlyOnValidSignature(t *testing.T) {
	w := &Wake{}
	a, b := w.Subscribe(), w.Subscribe()
	h := &Handler{Secret: []byte("s3cret"), Wake: w}

	post := func(sig string) int {
		req := httptest.NewRequest(http.MethodPost, "/hook", strings.NewReader(`{"ref":"main"}`))
		req.Header.Set("X-Gitea-Signature", sig)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	if code := post(sign("wrong", `{"ref":"main"}`)); code != http.StatusUnauthorized {
		t.Fatalf("wrong secret: %d", code)
	}
	if code := post(""); code != http.StatusUnauthorized {
		t.Fatalf("no signature: %d", code)
	}
	select {
	case <-a:
		t.Fatal("woken by a bad delivery")
	default:
	}
	// Two deliveries before the loop reads count as one wake-up.
	post(sign("s3cret", `{"ref":"main"}`))
	if code := post(sign("s3cret", `{"ref":"main"}`)); code != http.StatusNoContent {
		t.Fatalf("valid delivery: %d", code)
	}
	for _, c := range []<-chan struct{}{a, b} {
		select {
		case <-c:
		default:
			t.Fatal("subscriber not woken")
		}
		select {
		case <-c:
			t.Fatal("woken twice")
		default:
		}
	}
}

func TestSleepWakesEarly(t *testing.T) {
	w := &Wake{}
	c := w.Subscribe()
	w.Poke()
	start := time.Now()
	if !Sleep(context.Background(), time.Hour, c) || time.Since(start) > time.Second {
		t.Fatal("did not wake")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if Sleep(ctx, time.Hour, nil) {
		t.Fatal("ignored the context")
	}
}

func TestRegisterUpdatesExistingHook(t *testing.T) {
	var calls []string
	var sent map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		if u, p, _ := r.BasicAuth(); u != "installer" || p != "pw" {
			t.Errorf("credentials %q %q", u, p)
		}
		switch r.Method {
		case http.MethodGet:
			json.NewEncoder(w).Encode([]map[string]any{
				{"id": 3, "config": map[string]string{"url": "http://other/hook"}},
				{"id": 7, "config": map[string]string{"url": "http://controller_controller:8090/hook"}},
			})
		default:
			json.NewDecoder(r.Body).Decode(&sent)
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("{}"))
		}
	}))
	defer srv.Close()
	g := &Gitea{URL: srv.URL, User: "installer", Password: "pw"}
	if err := g.Register(context.Background(), "system", "http://controller_controller:8090/hook", []byte("abc")); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 || calls[1] != "PATCH /api/v1/orgs/system/hooks/7" {
		t.Fatalf("calls %v", calls)
	}
	if cfg := sent["config"].(map[string]any); cfg["secret"] != "abc" || cfg["url"] != "http://controller_controller:8090/hook" {
		t.Fatalf("config %v", cfg)
	}

	calls = nil
	if err := g.Register(context.Background(), "system", "http://controller_controlplane:8080/hook", []byte("abc")); err != nil {
		t.Fatal(err)
	}
	if calls[1] != "POST /api/v1/orgs/system/hooks" {
		t.Fatalf("calls %v", calls)
	}
}

func TestSecretIsStablePerTarget(t *testing.T) {
	a := Secret("pw", "http://controller_controller:8090/hook")
	if string(a) != string(Secret("pw", "http://controller_controller:8090/hook")) {
		t.Fatal("secret differs between instances")
	}
	if string(a) == string(Secret("pw", "http://controller_controlplane:8080/hook")) ||
		string(a) == string(Secret("other", "http://controller_controller:8090/hook")) {
		t.Fatal("secret does not depend on target and password")
	}
}
