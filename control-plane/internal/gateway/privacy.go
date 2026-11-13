package gateway

// Private repos. Everyone signed in reads every public repo; a private repo
// only its namespace's people read. Rights stay per namespace: there are no
// people of single repos. An agent that works on a private repo is private
// too: only the namespace's people see it, it answers only where only they
// read (a private chat, or the namespace's team channel), and its account
// acts only for people who may read the repo.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"homecloud/control-plane/internal/agents"
)

// privacyTTL is how long a repo's privacy is remembered: it is asked on
// every request to a private-looking agent.
const privacyTTL = 30 * time.Second

type privacy struct {
	mu   sync.Mutex
	seen map[string]privacyEntry
}

type privacyEntry struct {
	private bool
	at      time.Time
}

// RepoPrivate tells whether a repo is private, as the admin sees it. A repo
// that does not exist counts as private: it may be made so.
func (g *Gitea) RepoPrivate(ctx context.Context, org, repo string) (bool, error) {
	key := org + "/" + repo
	g.privacy.mu.Lock()
	if e, ok := g.privacy.seen[key]; ok && time.Since(e.at) < privacyTTL {
		g.privacy.mu.Unlock()
		return e.private, nil
	}
	g.privacy.mu.Unlock()
	if err := validOrg(org); err != nil {
		return true, err
	}
	if err := validRepo(repo); err != nil {
		return true, err
	}
	var r struct {
		Private bool `json:"private"`
	}
	status, err := g.do(ctx, "", http.MethodGet, fmt.Sprintf("/repos/%s/%s", org, repo), nil, &r)
	private := r.Private
	switch {
	case status == http.StatusNotFound:
		private = true
	case err != nil:
		return true, err
	}
	g.privacy.mu.Lock()
	if g.privacy.seen == nil {
		g.privacy.seen = map[string]privacyEntry{}
	}
	g.privacy.seen[key] = privacyEntry{private, time.Now()}
	g.privacy.mu.Unlock()
	return private, nil
}

// AgentPrivate tells whether an agent is for its namespace's people only:
// it says so (visible: members), or a repo it works on is private.
func (g *Gitea) AgentPrivate(ctx context.Context, a *agents.Agent) (bool, error) {
	if a.Visible == agents.VisibleMembers {
		return true, nil
	}
	for _, r := range a.WorksOn() {
		private, err := g.RepoPrivate(ctx, a.Namespace, r)
		if err != nil || private {
			return true, err
		}
	}
	return false, nil
}

// grantProblems are the grants of a namespace's agents that would let
// people outside it act on a private repo: for everyone on a private repo.
func (g *Gitea) grantProblems(ctx context.Context, ns string, list []agents.Agent) ([]string, error) {
	var out []string
	for _, a := range list {
		for i, gr := range a.Grants {
			if gr.For != agents.ForEveryone {
				continue
			}
			private, err := g.RepoPrivate(ctx, ns, gr.Repo)
			if err != nil {
				return nil, err
			}
			if private {
				out = append(out, fmt.Sprintf("%s/%s: grants[%d]: %s is private, so only %s's people may ask for changes to it; for: everyone is not possible there", agents.Dir, a.Name, i, gr.Repo, ns))
			}
		}
	}
	return out, nil
}

// mayRead refuses when the person a run is for may not read the repo: the
// agent's account never acts on a repo for someone who cannot see it.
func (t *Tools) mayRead(ctx context.Context, p Person, org, repo string) error {
	err := t.Gitea.CanRead(ctx, p.asker(), org, repo)
	if errors.Is(err, ErrForbidden) {
		return refused{fmt.Sprintf("%s cannot see %s/%s", p.asker(), org, repo)}
	}
	return err
}
