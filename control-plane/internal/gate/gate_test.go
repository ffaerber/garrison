package gate

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestTheGate(t *testing.T) {
	var seen []string
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Method+" "+r.URL.Path+" as "+r.Header.Get("X-Garrison-User")+" key "+r.Header.Get("X-Garrison-Ingress"))
		io.WriteString(w, "ok")
	}))
	defer app.Close()
	up, _ := url.Parse(app.URL)
	g := &Gate{Upstream: up, Key: "k3y", Header: "X-Garrison-Ingress", UserHeader: "X-Garrison-User",
		Writers: Writers("alice, carol"), ReadPosts: []string{"/api/login", "/api/renew"}, Closed: []string{"/api/share", "/api/public"}}
	do := func(method, path string, h map[string]string) int {
		r := httptest.NewRequest(method, path, strings.NewReader(""))
		for k, v := range h {
			r.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		g.ServeHTTP(w, r)
		return w.Code
	}
	as := func(who string) map[string]string {
		return map[string]string{"X-Garrison-Ingress": "k3y", "X-Auth-Request-Preferred-Username": who}
	}

	for _, c := range []struct {
		method, path string
		h            map[string]string
		code         int
	}{
		{"GET", "/api/resources/demo", as("bob"), 200},        // everyone signed in reads
		{"POST", "/api/login", as("bob"), 200},                // and signs in
		{"POST", "/api/resources/demo/x.png", as("bob"), 403}, // but bob is not the unit's
		{"DELETE", "/api/resources/demo/x.png", as("bob"), 403},
		{"PATCH", "/api/resources/demo/x.png", as("bob"), 403},
		{"POST", "/api/tus/demo/x.mp4", as("bob"), 403},
		{"POST", "/api/resources/demo/x.png", as("alice"), 200}, // alice is
		{"DELETE", "/api/resources/demo/x.png", as("carol"), 200},
		{"GET", "/api/share/demo", as("alice"), 403}, // no public links, for anyone
		{"POST", "/api/share/demo", as("alice"), 403},
		{"GET", "/api/../api/share/demo", as("alice"), 403},
		{"GET", "/api//public/dl/abc", as("alice"), 403},
		{"GET", "/api/resources/", map[string]string{"X-Auth-Request-Preferred-Username": "alice"}, 403}, // past Traefik: no key
		{"GET", "/api/resources/", map[string]string{"X-Garrison-Ingress": "wrong", "X-Auth-Request-Preferred-Username": "alice"}, 403},
		{"GET", "/api/resources/", map[string]string{"X-Garrison-Ingress": "k3y"}, 401}, // nobody signed in
	} {
		if got := do(c.method, c.path, c.h); got != c.code {
			t.Errorf("%s %s %v: %d, want %d", c.method, c.path, c.h, got, c.code)
		}
	}
	// The app hears who it is from the gate alone, never the key.
	seen = nil
	h := as("bob")
	h["X-Garrison-User"] = "alice"
	do("GET", "/api/resources/", h)
	if len(seen) != 1 || seen[0] != "GET /api/resources/ as bob key " {
		t.Errorf("app saw %v", seen)
	}
	seen = nil
	do("GET", "/api/x/../resources//demo", as("bob"))
	if len(seen) != 1 || !strings.HasPrefix(seen[0], "GET /api/resources/demo as bob") {
		t.Errorf("app saw %v", seen)
	}
	// The sign-in's user header when there is no preferred username.
	seen = nil
	do("GET", "/", map[string]string{"X-Garrison-Ingress": "k3y", "X-Auth-Request-User": "carol"})
	if len(seen) != 1 || !strings.Contains(seen[0], "as carol") {
		t.Errorf("app saw %v", seen)
	}
	// Without a key, the gate takes nothing.
	g.Key = ""
	if got := do("GET", "/", as("alice")); got != 403 {
		t.Errorf("no key: %d", got)
	}
}
