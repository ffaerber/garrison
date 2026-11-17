package hooks

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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
				{"id": 7, "config": map[string]string{"url": "http://control-plane_deployer:8090/hook"}},
			})
		default:
			json.NewDecoder(r.Body).Decode(&sent)
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("{}"))
		}
	}))
	defer srv.Close()
	g := &Gitea{URL: srv.URL, User: "installer", Password: "pw"}
	if err := g.Register(context.Background(), "garrison", "http://control-plane_deployer:8090/hook", []byte("abc")); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 || calls[1] != "PATCH /api/v1/orgs/garrison/hooks/7" {
		t.Fatalf("calls %v", calls)
	}
	if cfg := sent["config"].(map[string]any); cfg["secret"] != "abc" || cfg["url"] != "http://control-plane_deployer:8090/hook" {
		t.Fatalf("config %v", cfg)
	}

	calls = nil
	if err := g.Register(context.Background(), "garrison", "http://control-plane_gateway:8080/hook", []byte("abc")); err != nil {
		t.Fatal(err)
	}
	if calls[1] != "POST /api/v1/orgs/garrison/hooks" {
		t.Fatalf("calls %v", calls)
	}
}

func TestSecretIsStablePerTarget(t *testing.T) {
	a := Secret("pw", "http://control-plane_deployer:8090/hook")
	if string(a) != string(Secret("pw", "http://control-plane_deployer:8090/hook")) {
		t.Fatal("secret differs between instances")
	}
	if string(a) == string(Secret("pw", "http://control-plane_gateway:8080/hook")) ||
		string(a) == string(Secret("other", "http://control-plane_deployer:8090/hook")) {
		t.Fatal("secret does not depend on target and password")
	}
}

// The webhook goes onto every organisation, also those beyond Gitea's
// first page of 50.
func TestKeepRegistersOrganisationsOnLaterPages(t *testing.T) {
	var mu sync.Mutex
	hooked := map[string]bool{}
	gitea := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.URL.Path == "/api/v1/admin/orgs":
			var page []map[string]string
			switch r.URL.Query().Get("page") {
			case "1", "":
				for i := range 50 {
					page = append(page, map[string]string{"username": fmt.Sprintf("ns-%02d", i)})
				}
			case "2":
				page = append(page, map[string]string{"username": "photos"})
			}
			json.NewEncoder(w).Encode(page)
		case r.Method == http.MethodGet:
			w.Write([]byte("[]"))
		default:
			hooked[strings.Split(r.URL.Path, "/")[4]] = true
			w.Write([]byte("{}"))
		}
	}))
	defer gitea.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go (&Gitea{URL: gitea.URL, User: "admin", Password: "pw"}).Keep(ctx, "http://deployer/hook", []byte("s"), time.Hour, nil)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n, last := len(hooked), hooked["photos"]
		mu.Unlock()
		if n == 51 && last {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	t.Errorf("%d organisations got the webhook; photos: %v", len(hooked), hooked["photos"])
}

// Gitea tells nobody of a new organisation: Keep wakes the loops when it
// finds one, and not when nothing is new.
func TestKeepWakesTheLoopsForANewOrganisation(t *testing.T) {
	var mu sync.Mutex
	orgs := []string{"todo"}
	gitea := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.URL.Path == "/api/v1/admin/orgs":
			var page []map[string]string
			if p := r.URL.Query().Get("page"); p == "1" || p == "" {
				for _, o := range orgs {
					page = append(page, map[string]string{"username": o})
				}
			}
			json.NewEncoder(w).Encode(page)
		case r.Method == http.MethodGet:
			w.Write([]byte("[]"))
		default:
			w.Write([]byte("{}"))
		}
	}))
	defer gitea.Close()
	wake := &Wake{}
	woken := wake.Subscribe()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go (&Gitea{URL: gitea.URL, User: "admin", Password: "pw"}).Keep(ctx, "http://deployer/hook", []byte("s"), 20*time.Millisecond, wake)
	wait := func(want bool) {
		t.Helper()
		select {
		case <-woken:
			if !want {
				t.Fatal("woken with nothing new")
			}
		case <-time.After(200 * time.Millisecond):
			if want {
				t.Fatal("not woken")
			}
		}
	}
	wait(true) // the first pass finds todo
	wait(false)
	mu.Lock()
	orgs = append(orgs, "lab")
	mu.Unlock()
	wait(true)
}
