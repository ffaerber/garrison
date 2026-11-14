package agentapi

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"syscall"
	"time"

	"homecloud/control-plane/internal/agents"
	"homecloud/control-plane/internal/sandbox"
)

// InternetProxy is the sandboxes' only way out: an HTTP proxy (CONNECT for
// HTTPS, plain requests for HTTP) on the sandboxes' internal network. A
// sandbox names itself with its token, which counts only during a turn, and
// reaches what its agent's internet allows (agent.yaml), only on ports 80
// and 443, and never an address that is not public: not the home network,
// not the platform's services, not this machine.
type InternetProxy struct {
	Server *Server
	// Private tells whether an agent works on private repos; nil: the
	// server's Gitea, or none are.
	Private func(context.Context, *agents.Agent) (bool, error)
	// Allowed tells whether an address may be dialled; nil: Public.
	Allowed func(netip.Addr) bool
	// AnyPort lets every port through, not only 80 and 443 (tests).
	AnyPort bool
	// Tunnel is how long a CONNECT may stay open at most.
	Tunnel time.Duration
}

func (p *InternetProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c, ok := p.caller(r)
	if !ok {
		w.Header().Set("Proxy-Authenticate", `Basic realm="homecloud sandbox"`)
		http.Error(w, "not a sandbox in a turn", http.StatusProxyAuthRequired)
		return
	}
	target, err := p.target(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if why := p.refuse(r.Context(), c, target); why != "" {
		log.Printf("agents: internet: %s: %s: %s refused: %s", c.Person, c.Agent, target, why)
		http.Error(w, "refused by the home cloud: "+why, http.StatusForbidden)
		return
	}
	log.Printf("agents: internet: %s: %s: %s %s", c.Person, c.Agent, r.Method, target)
	if r.Method == http.MethodConnect {
		p.tunnel(w, r, target)
		return
	}
	p.forward(w, r)
}

func (p *InternetProxy) caller(r *http.Request) (sandbox.Caller, bool) {
	raw, ok := strings.CutPrefix(r.Header.Get("Proxy-Authorization"), "Basic ")
	if !ok || p.Server.Sandboxes == nil {
		return sandbox.Caller{}, false
	}
	dec, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return sandbox.Caller{}, false
	}
	_, token, _ := strings.Cut(string(dec), ":")
	return p.Server.Sandboxes.Caller(token)
}

// target is host:port of the request.
func (p *InternetProxy) target(r *http.Request) (string, error) {
	if r.Method == http.MethodConnect {
		host, port, err := net.SplitHostPort(r.Host)
		if err != nil || host == "" {
			return "", fmt.Errorf("CONNECT needs host:port")
		}
		return net.JoinHostPort(strings.ToLower(host), port), nil
	}
	if r.URL.Scheme != "http" || r.URL.Host == "" {
		return "", fmt.Errorf("not a proxy request: use this as HTTP_PROXY and HTTPS_PROXY")
	}
	host, port := r.URL.Hostname(), r.URL.Port()
	if port == "" {
		port = "80"
	}
	return net.JoinHostPort(strings.ToLower(host), port), nil
}

// refuse says why the caller's agent may not reach target, or "".
func (p *InternetProxy) refuse(ctx context.Context, c sandbox.Caller, target string) string {
	host, port, _ := net.SplitHostPort(target)
	if port != "443" && port != "80" && !p.AnyPort {
		return "only ports 80 and 443"
	}
	a, ok, err := p.Server.Agents.Get(ctx, c.Agent)
	if err != nil || !ok {
		return "the agent is not known"
	}
	internet := a.Internet
	if internet.Wide() {
		private := p.Private
		if private == nil && p.Server.Gitea != nil {
			private = p.Server.Gitea.AgentPrivate
		}
		if private != nil {
			// A failed check counts as private.
			if yes, err := private(ctx, &a); yes || err != nil {
				internet = internet.Narrowed()
			}
		}
	}
	if _, err := netip.ParseAddr(host); err == nil && !internet.All {
		return "addresses only with internet: all; use the host's name"
	}
	if !internet.Allows(host) {
		return fmt.Sprintf("%s reaches %s; ask an admin of %s to add %s to internet in its agent.yaml", c.Agent, internet, a.Namespace, host)
	}
	return ""
}

// Public is whether an address is on the internet: not loopback, private,
// link-local, shared (CGNAT), multicast or otherwise reserved.
func Public(a netip.Addr) bool {
	a = a.Unmap()
	if !a.IsGlobalUnicast() || a.IsPrivate() || a.IsLoopback() || a.IsLinkLocalUnicast() {
		return false
	}
	for _, p := range reserved {
		if p.Contains(a) {
			return false
		}
	}
	return true
}

var reserved = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"), // shared address space, e.g. Tailscale
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("64:ff9b:1::/48"),
	netip.MustParsePrefix("2001:db8::/32"),
}

var errNotPublic = errors.New("not a public address")

// dial connects to target, checking the address it actually connects to,
// so a name that resolves to the home network is refused too.
func (p *InternetProxy) dial(ctx context.Context, network, target string) (net.Conn, error) {
	allowed := p.Allowed
	if allowed == nil {
		allowed = Public
	}
	d := net.Dialer{
		Timeout: 15 * time.Second,
		Control: func(_, address string, _ syscall.RawConn) error {
			ap, err := netip.ParseAddrPort(address)
			if err != nil || !allowed(ap.Addr()) {
				return fmt.Errorf("%s: %w", address, errNotPublic)
			}
			return nil
		},
	}
	return d.DialContext(ctx, network, target)
}

func (p *InternetProxy) tunnel(w http.ResponseWriter, r *http.Request, target string) {
	up, err := p.dial(r.Context(), "tcp", target)
	if err != nil {
		status := http.StatusBadGateway
		if errors.Is(err, errNotPublic) {
			status = http.StatusForbidden
		}
		http.Error(w, err.Error(), status)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		up.Close()
		http.Error(w, "cannot tunnel", http.StatusInternalServerError)
		return
	}
	down, buf, err := hj.Hijack()
	if err != nil {
		up.Close()
		return
	}
	limit := p.Tunnel
	if limit <= 0 {
		limit = time.Hour
	}
	deadline := time.Now().Add(limit)
	up.SetDeadline(deadline)
	down.SetDeadline(deadline)
	down.Write([]byte("HTTP/1.1 200 Connection established\r\n\r\n"))
	done := make(chan struct{}, 2)
	go func() {
		// What the client sent after its request, then the rest.
		if n := buf.Reader.Buffered(); n > 0 {
			data, _ := buf.Reader.Peek(n)
			up.Write(data)
		}
		io.Copy(up, down)
		if c, ok := up.(interface{ CloseWrite() error }); ok {
			c.CloseWrite()
		}
		done <- struct{}{}
	}()
	go func() {
		io.Copy(down, up)
		if c, ok := down.(interface{ CloseWrite() error }); ok {
			c.CloseWrite()
		}
		done <- struct{}{}
	}()
	<-done
	<-done
	up.Close()
	down.Close()
}

// hop are the headers of one connection, not passed on.
var hop = []string{"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade"}

func (p *InternetProxy) forward(w http.ResponseWriter, r *http.Request) {
	out := r.Clone(r.Context())
	out.RequestURI = ""
	for _, h := range hop {
		out.Header.Del(h)
	}
	t := &http.Transport{DialContext: p.dial, Proxy: nil, ResponseHeaderTimeout: time.Minute}
	defer t.CloseIdleConnections()
	resp, err := t.RoundTrip(out)
	if err != nil {
		status := http.StatusBadGateway
		if errors.Is(err, errNotPublic) {
			status = http.StatusForbidden
		}
		http.Error(w, err.Error(), status)
		return
	}
	defer resp.Body.Close()
	for _, h := range hop {
		resp.Header.Del(h)
	}
	for k, v := range resp.Header {
		w.Header()[k] = v
	}
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}
