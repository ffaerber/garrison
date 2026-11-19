// Package gate stands in front of an app that believes a header naming the
// person signed in, such as Filebrowser's proxy sign-in, which cannot check
// Garrison's ingress key itself. The gate takes only requests that came
// through Traefik's sign-in (they carry the environment's ingress key),
// names the person to the app in a header of its own, which nobody can send
// past it, and lets only the namespace's people change anything.
package gate

import (
	"crypto/subtle"
	"net/http"
	"net/http/httputil"
	"net/url"
	"slices"
	"strings"
)

// The sign-in's headers, as Traefik passes them from oauth2-proxy; the
// first one set names the person.
var signInHeaders = []string{"X-Auth-Request-Preferred-Username", "X-Auth-Request-User"}

// Gate proxies to the app.
type Gate struct {
	Upstream *url.URL
	// Key is the environment's ingress key; Header the header it comes in
	// (render.IngressHeader).
	Key, Header string
	// UserHeader is the header the app reads the person from.
	UserHeader string
	// Writers may change things; everyone else signed in only reads.
	Writers []string
	// ReadPosts are paths everyone may POST to, as reading needs them, e.g.
	// a sign-in that makes a session (/api/login).
	ReadPosts []string
	// Closed are path prefixes nobody reaches, e.g. public share links,
	// which would open files to people not signed in.
	Closed []string

	proxy *httputil.ReverseProxy
}

func (g *Gate) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if g.proxy == nil {
		g.proxy = httputil.NewSingleHostReverseProxy(g.Upstream)
	}
	if g.Key == "" || subtle.ConstantTimeCompare([]byte(r.Header.Get(g.Header)), []byte(g.Key)) != 1 {
		http.Error(w, "only through the installation's sign-in", http.StatusForbidden)
		return
	}
	who := ""
	for _, h := range signInHeaders {
		if v := strings.TrimSpace(r.Header.Get(h)); v != "" {
			who = v
			break
		}
	}
	if who == "" {
		http.Error(w, "nobody is signed in", http.StatusUnauthorized)
		return
	}
	p := cleanPath(r.URL.Path)
	for _, c := range g.Closed {
		if strings.TrimSuffix(p, "/") == c || strings.HasPrefix(p, strings.TrimSuffix(c, "/")+"/") {
			http.Error(w, "not here: everything is only for people signed in", http.StatusForbidden)
			return
		}
	}
	reads := r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions ||
		(r.Method == http.MethodPost && slices.Contains(g.ReadPosts, strings.TrimSuffix(p, "/")))
	if !reads && !slices.Contains(g.Writers, who) {
		http.Error(w, "only the namespace's people change files here; "+who+" may look and download", http.StatusForbidden)
		return
	}
	// The app gets the path the gate decided on.
	r.URL.Path, r.URL.RawPath = p, ""
	r.Header.Del(g.UserHeader)
	r.Header.Set(g.UserHeader, who)
	r.Header.Del(g.Header)
	g.proxy.ServeHTTP(w, r)
}

// cleanPath is the path as the app sees it: without dot segments and
// doubled slashes, so /api/../api/share is /api/share.
func cleanPath(p string) string {
	var out []string
	for _, seg := range strings.Split(p, "/") {
		switch seg {
		case "", ".":
		case "..":
			if len(out) > 0 {
				out = out[:len(out)-1]
			}
		default:
			out = append(out, seg)
		}
	}
	clean := "/" + strings.Join(out, "/")
	// A folder's trailing slash stays: the app may tell folders by it.
	if strings.HasSuffix(p, "/") && clean != "/" {
		clean += "/"
	}
	return clean
}

// Writers reads a comma-separated list, as {{members}} gives it.
func Writers(s string) []string {
	var out []string
	for _, w := range strings.Split(s, ",") {
		if w = strings.TrimSpace(w); w != "" {
			out = append(out, w)
		}
	}
	return out
}
