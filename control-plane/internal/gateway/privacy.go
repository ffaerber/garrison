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
	"strings"
	"sync"
	"time"

	"homecloud/control-plane/internal/agents"
	"homecloud/control-plane/internal/stack"
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

// stackProblems checks the stacks of a namespace's config repo at ref, as
// the deployer will read them: stacks/<name>.yaml, each valid, its source a
// repo of the namespace. system has none: it holds the platform, not apps.
func (g *Gitea) stackProblems(ctx context.Context, ns, ref string) ([]string, error) {
	f := configFiles{g: g, ns: ns, ref: ref}
	entries, err := f.List(ctx, stack.Dir)
	if errors.Is(err, agents.ErrNotExist) {
		entries, err = nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	parsed := map[string]*stack.Stack{}
	for _, e := range entries {
		path := stack.Dir + "/" + e.Name
		name, ok := strings.CutSuffix(e.Name, ".yaml")
		switch {
		case ns == SystemNamespace:
			out = append(out, path+": system holds the platform, not apps; put the app into a namespace of its own")
			continue
		case e.Type != "file" || !ok:
			out = append(out, path+": "+stack.Dir+"/ holds only <name>.yaml files")
			continue
		}
		data, err := f.Read(ctx, path)
		if err != nil {
			return nil, err
		}
		s, err := stack.ParseIn([]byte(data), ns, name)
		if err != nil {
			out = append(out, path+": "+err.Error())
			continue
		}
		parsed[name] = s
		if s.Source != "" {
			status, err := g.do(ctx, "", http.MethodGet, fmt.Sprintf("/repos/%s/%s", ns, s.Source), nil, nil)
			if status == http.StatusNotFound {
				out = append(out, fmt.Sprintf("%s: there is no repo %s/%s to be its source", path, ns, s.Source))
			} else if err != nil {
				return nil, err
			}
		}
	}
	more, err := restoreProblems(ctx, f, parsed)
	return append(out, more...), err
}

// restoreProblems checks the restore requests, restores/<name>.yaml: each
// names a valid stack of the namespace and one of its environments. Whether
// the snapshot exists, the deployer tells.
func restoreProblems(ctx context.Context, f configFiles, stacks map[string]*stack.Stack) ([]string, error) {
	entries, err := f.List(ctx, stack.RestoreDir)
	if errors.Is(err, agents.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		path := stack.RestoreDir + "/" + e.Name
		if e.Type != "file" || !strings.HasSuffix(e.Name, ".yaml") {
			out = append(out, path+": "+stack.RestoreDir+"/ holds only <name>.yaml files")
			continue
		}
		data, err := f.Read(ctx, path)
		if err != nil {
			return nil, err
		}
		r, err := stack.ParseRestore([]byte(data))
		if err != nil {
			out = append(out, path+": "+strings.ReplaceAll(err.Error(), "\n", "; "))
			continue
		}
		s, ok := stacks[r.Stack]
		if !ok {
			out = append(out, fmt.Sprintf("%s: there is no valid stack %s in %s/", path, r.Stack, stack.Dir))
			continue
		}
		// Previews keep no snapshots.
		if _, ok := s.Environments[r.Environment]; !ok || r.Environment == stack.PreviewEnvironment {
			out = append(out, fmt.Sprintf("%s: %s has no environment %s whose data can be restored", path, r.Stack, r.Environment))
		}
	}
	return out, nil
}
