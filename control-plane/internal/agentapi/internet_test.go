package agentapi

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"

	"garrison/control-plane/internal/agents"
	"garrison/control-plane/internal/gateway"
	"garrison/control-plane/internal/sandbox"
)

// withInternet gives s's sandboxes the proxy. everywhere lets it dial this
// machine, as the test's sites are here; private says which agents work on
// private repos.
func withInternet(t *testing.T, s *Server, everywhere bool, private ...string) {
	t.Helper()
	p := &InternetProxy{Server: s, AnyPort: true, Private: func(_ context.Context, a *agents.Agent) (bool, error) {
		for _, id := range private {
			if a.ID() == id {
				return true, nil
			}
		}
		return false, nil
	}}
	if everywhere {
		p.Allowed = func(netip.Addr) bool { return true }
	}
	ps := httptest.NewServer(p)
	t.Cleanup(ps.Close)
	s.Sandboxes.Proxy = ps.URL
}

func site(t *testing.T, tls bool) string {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("hello from the internet")) })
	var srv *httptest.Server
	if tls {
		srv = httptest.NewTLSServer(h)
	} else {
		srv = httptest.NewServer(h)
	}
	t.Cleanup(srv.Close)
	return srv.URL
}

func agentsWith(s *Server, list ...agents.Agent) {
	s.Agents = append(s.Agents.(fakeAgents), list...)
}

func TestAnAgentReachesWhatItsInternetAllows(t *testing.T) {
	s, _, _ := setup(t)
	agentsWith(s,
		agents.Agent{Unit: "homepage", Name: "web", Prompt: "p", Internet: agents.Internet{All: true}},
		agents.Agent{Unit: "homepage", Name: "builder", Prompt: "p", Internet: agents.Internet{Packages: true}},
		agents.Agent{Unit: "homepage", Name: "secret", Prompt: "p", Internet: agents.Internet{All: true}},
	)
	d := withSandboxes(t, s)
	withInternet(t, s, true, "homepage/secret")
	https, plain := site(t, true), site(t, false)

	for _, u := range []string{https, plain} {
		_, got := chatIn(t, s, "c1", "homepage/web", Message{Role: "user", Content: "fetch " + u})
		if got != "fetched: 200 hello from the internet" {
			t.Errorf("internet: all, %s: %q", u, got)
		}
	}
	// The registries only: an address is refused, as is a name not on the list.
	_, got := chatIn(t, s, "c2", "homepage/builder", Message{Role: "user", Content: "fetch " + https})
	if !strings.Contains(got, "403") && !strings.Contains(got, "Forbidden") {
		t.Errorf("packages, an address: %q", got)
	}
	_, got = chatIn(t, s, "c2", "homepage/builder", Message{Role: "user", Content: "fetch http://example.org/"})
	if !strings.Contains(got, "403") || !strings.Contains(got, "ask a root admin to add example.org to builder's internet in garrison/config") {
		t.Errorf("packages, example.org: %q", got)
	}
	// An agent on private repos keeps the registries only.
	_, got = chatIn(t, s, "c3", "homepage/secret", Message{Role: "user", Content: "fetch " + plain})
	if !strings.Contains(got, "403") {
		t.Errorf("private agent, internet: all: %q", got)
	}
	// An agent without internet is refused everything.
	_, got = chatIn(t, s, "c4", "homepage/reader", Message{Role: "user", Content: "fetch http://pypi.org/"})
	if !strings.Contains(got, "403") || !strings.Contains(got, "no internet") {
		t.Errorf("no internet: %q", got)
	}
	// Each sandbox holds its own token in the proxy's address, nothing else.
	for _, c := range d.Containers {
		for _, e := range c.Env {
			if strings.HasPrefix(e, "HTTPS_PROXY=") && !strings.Contains(e, "sandbox:") {
				t.Errorf("proxy without the token: %s", e)
			}
		}
	}
}

func TestTheProxyRefusesTheHomeNetwork(t *testing.T) {
	s, _, _ := setup(t)
	agentsWith(s, agents.Agent{Unit: "homepage", Name: "web", Prompt: "p", Internet: agents.Internet{All: true}})
	withSandboxes(t, s)
	withInternet(t, s, false) // only public addresses
	for _, u := range []string{site(t, true), site(t, false)} {
		_, got := chatIn(t, s, "c1", "homepage/web", Message{Role: "user", Content: "fetch " + u})
		if !strings.Contains(got, "403") && !strings.Contains(got, "Forbidden") {
			t.Errorf("%s: %q", u, got)
		}
	}
}

func TestOnlyPorts80And443(t *testing.T) {
	s, _, _ := setup(t)
	agentsWith(s, agents.Agent{Unit: "homepage", Name: "web", Prompt: "p", Internet: agents.Internet{All: true}})
	p := &InternetProxy{Server: s}
	c := sandbox.Caller{Agent: "homepage/web", Person: "alice"}
	for target, want := range map[string]string{"example.org:443": "", "example.org:80": "", "example.org:22": "only ports 80 and 443", "example.org:8443": "only ports 80 and 443"} {
		if got := p.refuse(context.Background(), c, target); got != want {
			t.Errorf("%s: %q", target, got)
		}
	}
}

func TestTheProxyWantsASandboxInATurn(t *testing.T) {
	s, _, _ := setup(t)
	withSandboxes(t, s)
	p := &InternetProxy{Server: s}
	for _, auth := range []string{"", "Basic c2FuZGJveDpub3BlCg=="} {
		r := httptest.NewRequest(http.MethodConnect, "pypi.org:443", nil)
		r.Host = "pypi.org:443"
		if auth != "" {
			r.Header.Set("Proxy-Authorization", auth)
		}
		w := httptest.NewRecorder()
		p.ServeHTTP(w, r)
		if w.Code != http.StatusProxyAuthRequired {
			t.Errorf("auth %q: %d", auth, w.Code)
		}
	}
}

func TestPublic(t *testing.T) {
	for addr, want := range map[string]bool{
		"1.1.1.1": true, "2606:4700::1111": true,
		"127.0.0.1": false, "10.0.7.3": false, "192.168.1.1": false, "172.17.0.1": false,
		"100.100.1.1": false, "169.254.169.254": false, "::1": false, "fd00::1": false,
		"fe80::1": false, "0.0.0.0": false, "::ffff:192.168.1.1": false, "224.0.0.1": false,
	} {
		if got := Public(netip.MustParseAddr(addr)); got != want {
			t.Errorf("%s: %v", addr, got)
		}
	}
}

func TestAWideAgentClonesOnlyPublicRepos(t *testing.T) {
	gitea := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/repos/homepage/todo":
			io.WriteString(w, `{"private":false}`)
		case "/api/v1/repos/homepage/secret":
			io.WriteString(w, `{"private":true}`)
		default:
			io.WriteString(w, "refs")
		}
	}))
	defer gitea.Close()
	g := &GitProxy{Gitea: &gateway.Gitea{URL: gitea.URL, User: "admin", Password: "pw"}, Agents: fakeAgents{
		{Unit: "homepage", Name: "web", Internet: agents.Internet{Domains: []string{"example.org"}}},
		{Unit: "homepage", Name: "builder", Internet: agents.Internet{Packages: true}},
	}}
	for _, c := range []struct {
		agent, repo string
		want        int
	}{
		{"homepage/web", "todo", 200},
		{"homepage/web", "secret", http.StatusForbidden},
		{"homepage/builder", "secret", 200}, // the registries only: it may
	} {
		h := g.Handler(func(http.ResponseWriter, *http.Request) (sandbox.Caller, bool) {
			return sandbox.Caller{Agent: c.agent, Person: "alice", Token: "kc"}, true
		})
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", "/git/homepage/"+c.repo+".git/info/refs?service=git-upload-pack", nil))
		if w.Code != c.want {
			t.Errorf("%s clones %s: %d %s", c.agent, c.repo, w.Code, w.Body)
		}
	}
}

// INTERNET_ALLOW, for tests: the ranges given are let through as if they
// were public; everything else private stays refused.
func TestPublicOr(t *testing.T) {
	allowed, ranges, err := PublicOr("10.43.0.0/16, 192.168.5.7/32")
	if err != nil || len(ranges) != 2 {
		t.Fatal(err, ranges)
	}
	for addr, want := range map[string]bool{
		"10.43.12.1":    true,
		"192.168.5.7":   true,
		"192.168.5.8":   false,
		"10.42.0.5":     false,
		"127.0.0.1":     false,
		"93.184.216.34": true,
	} {
		if got := allowed(netip.MustParseAddr(addr)); got != want {
			t.Errorf("%s: %v, want %v", addr, got, want)
		}
	}
	if _, _, err := PublicOr("everything"); err == nil {
		t.Error("a word was taken for a range")
	}
}

// The gateway's browser for the public web names the agent browsing with a
// password only the service key makes; the proxy then holds it to that
// agent's internet, as a sandbox of it.
func TestTheProxyGoesOutForThePublicWebsBrowser(t *testing.T) {
	s, _, _ := setup(t)
	agentsWith(s,
		agents.Agent{Unit: "homepage", Name: "web", Prompt: "p", Internet: agents.Internet{All: true}},
		agents.Agent{Unit: "homepage", Name: "builder", Prompt: "p", Internet: agents.Internet{Packages: true}},
	)
	s.Gateway.ServiceKey = "service-key"
	p := &InternetProxy{Server: s, AnyPort: true, Allowed: func(netip.Addr) bool { return true }}
	plain := site(t, false)
	get := func(user, password string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, plain+"/", nil)
		r.SetBasicAuth(user, password)
		r.Header.Set("Proxy-Authorization", r.Header.Get("Authorization"))
		r.Header.Del("Authorization")
		w := httptest.NewRecorder()
		p.ServeHTTP(w, r)
		return w
	}
	if w := get("homepage/web", gateway.BrowserProxyPassword("service-key", "homepage/web")); w.Code != 200 || !strings.Contains(w.Body.String(), "hello from the internet") {
		t.Errorf("the browser as web: %d %s", w.Code, w.Body)
	}
	if w := get("homepage/builder", gateway.BrowserProxyPassword("service-key", "homepage/builder")); w.Code != http.StatusForbidden {
		t.Errorf("the browser as builder, whose internet is the registries: %d", w.Code)
	}
	for name, auth := range map[string][2]string{
		"another agent's password": {"homepage/builder", gateway.BrowserProxyPassword("service-key", "homepage/web")},
		"another key":              {"homepage/web", gateway.BrowserProxyPassword("other-key", "homepage/web")},
		"no user":                  {"", gateway.BrowserProxyPassword("service-key", "")},
	} {
		if w := get(auth[0], auth[1]); w.Code == 200 {
			t.Errorf("%s: let through", name)
		}
	}
	s.Gateway.ServiceKey = "" // without a key, nothing is the browser
	if w := get("homepage/web", gateway.BrowserProxyPassword("", "homepage/web")); w.Code != http.StatusProxyAuthRequired {
		t.Errorf("no service key: %d", w.Code)
	}
}
