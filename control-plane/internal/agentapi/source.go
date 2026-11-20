package agentapi

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"garrison/control-plane/internal/agents"
	"garrison/control-plane/internal/gateway"
)

// GiteaAgents reads the agents as they work in each unit: garrison's,
// with each unit's grants and routines, on main.
// A chat reads its agent afresh, so a merged change applies to the next
// message; the list of all agents is kept for a few seconds, as Open WebUI
// asks for it often.
type GiteaAgents struct {
	Gitea *gateway.Gitea
	TTL   time.Duration // of the list; 0: 15 s

	mu   sync.Mutex
	list []agents.Agent
	at   time.Time
}

func (g *GiteaAgents) All(ctx context.Context) ([]agents.Agent, error) {
	ttl := g.TTL
	if ttl == 0 {
		ttl = 15 * time.Second
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.list != nil && time.Since(g.at) < ttl {
		return g.list, nil
	}
	units, err := g.Gitea.Units(ctx, "")
	if err != nil {
		return nil, err
	}
	defs, err := g.Gitea.Definitions(ctx)
	if err != nil {
		return nil, err
	}
	list := []agents.Agent{}
	for _, ns := range units {
		found, err := g.Gitea.AgentsWith(ctx, defs, ns.Name)
		if err != nil {
			return nil, err
		}
		list = append(list, found...)
	}
	g.list, g.at = list, time.Now()
	return list, nil
}

func (g *GiteaAgents) Get(ctx context.Context, id string) (agents.Agent, bool, error) {
	ns, _, ok := strings.Cut(id, "/")
	if !ok {
		return agents.Agent{}, false, nil
	}
	found, _, err := g.Gitea.Agents(ctx, ns, "main")
	if errors.Is(err, gateway.ErrInvalid) {
		return agents.Agent{}, false, nil
	}
	if err != nil {
		return agents.Agent{}, false, err
	}
	for _, a := range found {
		if a.ID() == id {
			return a, true, nil
		}
	}
	return agents.Agent{}, false, nil
}
