package agentapi

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"garrison/control-plane/internal/gateway"
	"garrison/control-plane/internal/sandbox"
)

func TestGitFetchesOnlyWhatThePersonMayRead(t *testing.T) {
	var reached []string
	gitea := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, p, _ := r.BasicAuth(); u != "admin" || p != "pw" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/api/v1/repos/homepage/todo":
			io.WriteString(w, "{}")
		case "/api/v1/repos/garrison/control-plane":
			if r.Header.Get("Sudo") != "root" {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			io.WriteString(w, "{}")
		default:
			reached = append(reached, r.Method+" "+r.URL.Path+"?"+r.URL.RawQuery)
			io.WriteString(w, "refs")
		}
	}))
	defer gitea.Close()
	g := &GitProxy{Gitea: &gateway.Gitea{URL: gitea.URL, User: "admin", Password: "pw"}}
	inTurn := true
	h := g.Handler(func(w http.ResponseWriter, r *http.Request) (sandbox.Caller, bool) {
		if !inTurn || r.Header.Get("Authorization") != "Bearer sb" {
			w.WriteHeader(http.StatusUnauthorized)
			return sandbox.Caller{}, false
		}
		return sandbox.Caller{Agent: "homepage/dev", Person: "bob", Token: "kc"}, true
	})
	do := func(method, path string) (int, string) {
		r := httptest.NewRequest(method, path, strings.NewReader(""))
		r.Header.Set("Authorization", "Bearer sb")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code, w.Body.String()
	}
	if code, body := do("GET", "/git/homepage/todo.git/info/refs?service=git-upload-pack"); code != 200 || body != "refs" {
		t.Fatalf("clone: %d %q", code, body)
	}
	if code, _ := do("POST", "/git/homepage/todo.git/git-upload-pack"); code != 200 {
		t.Fatalf("fetch: %d", code)
	}
	if strings.Join(reached, ",") != "GET /homepage/todo.git/info/refs?service=git-upload-pack,POST /homepage/todo.git/git-upload-pack?" {
		t.Errorf("reached Gitea: %v", reached)
	}
	reached = nil
	for _, c := range []struct{ method, path string }{
		{"GET", "/git/garrison/control-plane.git/info/refs?service=git-upload-pack"}, // bob may not read it
		{"GET", "/git/homepage/todo.git/info/refs?service=git-receive-pack"},         // a push
		{"POST", "/git/homepage/todo.git/git-receive-pack"},
		{"GET", "/git/homepage/todo.git/HEAD"},
		{"GET", "/git/../todo.git/info/refs?service=git-upload-pack"},
	} {
		if code, _ := do(c.method, c.path); code < 400 {
			t.Errorf("%s %s: %d", c.method, c.path, code)
		}
	}
	inTurn = false
	if code, _ := do("GET", "/git/homepage/todo.git/info/refs?service=git-upload-pack"); code != http.StatusUnauthorized {
		t.Errorf("between turns: %d", code)
	}
	if len(reached) != 0 {
		t.Errorf("reached Gitea: %v", reached)
	}
}
