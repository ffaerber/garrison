package agentapi

import (
	"errors"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"regexp"

	"homecloud/control-plane/internal/gateway"
	"homecloud/control-plane/internal/sandbox"
)

// GitProxy lets sandboxes clone and fetch repos over Git's HTTP protocol,
// read-only: only fetches, only of repos the person whose turn runs may
// read, and only during a turn. Gitea is asked with the admin account, as
// Gitea's Git protocol knows no Sudo; the person's rights are checked first.
// Changes go back as patches, through the gateway's propose_patch, as the
// person.
type GitProxy struct {
	Gitea *gateway.Gitea
	// Agents, if set, keeps private repos from agents whose sandbox reaches
	// more of the internet than the package registries.
	Agents Agents
}

// /git/<namespace>/<repo>[.git]/info/refs?service=git-upload-pack or
// /git/<namespace>/<repo>[.git]/git-upload-pack.
var gitPath = regexp.MustCompile(`^/git/([^/]+)/([^/]+?)(?:\.git)?/(info/refs|git-upload-pack)$`)

// Handler serves /git/; caller names the sandbox's person.
func (g *GitProxy) Handler(caller func(http.ResponseWriter, *http.Request) (sandbox.Caller, bool)) http.Handler {
	target, _ := url.Parse(g.Gitea.URL)
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.Host = target.Host
			pr.Out.Header.Del("Authorization")
			pr.Out.SetBasicAuth(g.Gitea.User, g.Gitea.Password)
		},
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m := gitPath.FindStringSubmatch(r.URL.Path)
		if m == nil {
			http.Error(w, "not a Git fetch: clone <api>/git/<namespace>/<repo>.git", http.StatusNotFound)
			return
		}
		org, repo, op := m[1], m[2], m[3]
		upload := (op == "info/refs" && r.Method == http.MethodGet && r.URL.Query().Get("service") == "git-upload-pack") ||
			(op == "git-upload-pack" && r.Method == http.MethodPost)
		if !upload {
			http.Error(w, "this Git is read-only: propose changes with the tool propose_workspace_changes", http.StatusForbidden)
			return
		}
		c, ok := caller(w, r)
		if !ok {
			return
		}
		if err := g.Gitea.CanRead(r.Context(), c.Person, org, repo); err != nil {
			status := http.StatusBadGateway
			if errors.Is(err, gateway.ErrForbidden) || errors.Is(err, gateway.ErrInvalid) {
				status = http.StatusNotFound // as Gitea: what one may not read does not exist
			}
			log.Printf("agents: git: %s: %s: %s/%s: %v", c.Person, c.Agent, org, repo, err)
			http.Error(w, "no repo "+org+"/"+repo+" that "+c.Person+" may read", status)
			return
		}
		if g.Agents != nil {
			a, ok, err := g.Agents.Get(r.Context(), c.Agent)
			if err != nil || !ok {
				http.Error(w, "the agent is not known", http.StatusBadGateway)
				return
			}
			if a.Internet.Wide() {
				private, err := g.Gitea.RepoPrivate(r.Context(), org, repo)
				if private || err != nil {
					log.Printf("agents: git: %s: %s: %s/%s refused: private, and the agent reaches the internet", c.Person, c.Agent, org, repo)
					http.Error(w, org+"/"+repo+" is private, and "+c.Agent+" reaches more of the internet than the package registries: it clones only public repos", http.StatusForbidden)
					return
				}
			}
		}
		if op == "info/refs" {
			log.Printf("agents: git: %s: %s: fetch %s/%s", c.Person, c.Agent, org, repo)
		}
		r2 := r.Clone(r.Context())
		r2.URL.Path = "/" + org + "/" + repo + ".git/" + op
		r2.URL.RawPath = ""
		proxy.ServeHTTP(w, r2)
	})
}
