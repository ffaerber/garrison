package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"homecloud/control-plane/internal/agents"
)

// ToolNames are the tools an agent's agent.yaml may list. load_skill and
// read_skill_file come with skills and are not listed.
func ToolNames() []string {
	var out []string
	for _, s := range baseSpecs() {
		out = append(out, s.Name)
	}
	return out
}

// Catalog holds every namespace's agents, as the Open WebUI sync last read
// them, for the MCP endpoints to look up. An agent it does not know yet, e.g.
// before the first sync, is read from Gitea when it is asked for.
type Catalog struct {
	Gitea *Gitea

	mu     sync.RWMutex
	agents map[string]agents.Agent // by ID
}

// Set replaces the catalog.
func (c *Catalog) Set(list []agents.Agent) {
	m := make(map[string]agents.Agent, len(list))
	for _, a := range list {
		m[a.ID()] = a
	}
	c.mu.Lock()
	c.agents = m
	c.mu.Unlock()
}

// Get looks an agent up by its ID, <namespace>/<agent>.
func (c *Catalog) Get(id string) (agents.Agent, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	a, ok := c.agents[id]
	return a, ok
}

// Lookup is Get, reading the agent's namespace from Gitea if it is unknown.
func (c *Catalog) Lookup(ctx context.Context, id string) (agents.Agent, bool) {
	if a, ok := c.Get(id); ok || c.Gitea == nil {
		return a, ok
	}
	ns, _, ok := strings.Cut(id, "/")
	if !ok || validOrg(ns) != nil {
		return agents.Agent{}, false
	}
	found, _, err := c.Gitea.Agents(ctx, ns, "main")
	if err != nil {
		log.Printf("agents of %s: %v", ns, err)
		return agents.Agent{}, false
	}
	c.mu.Lock()
	if c.agents == nil {
		c.agents = map[string]agents.Agent{}
	}
	var out agents.Agent
	ok = false
	for _, a := range found {
		c.agents[a.ID()] = a
		if a.ID() == id {
			out, ok = a, true
		}
	}
	c.mu.Unlock()
	return out, ok
}

// configFiles reads a namespace's config repo at ref, as the admin.
type configFiles struct {
	g   *Gitea
	ns  string
	ref string
}

func (f configFiles) List(ctx context.Context, dir string) ([]agents.Entry, error) {
	var raw json.RawMessage
	status, err := f.g.do(ctx, "", http.MethodGet, fmt.Sprintf("/repos/%s/%s/contents/%s?ref=%s", f.ns, ConfigRepo, escapePath(dir), url.QueryEscape(f.ref)), nil, &raw)
	if status == http.StatusNotFound {
		return nil, agents.ErrNotExist
	}
	if err != nil {
		return nil, err
	}
	var list []Entry
	if json.Unmarshal(raw, &list) != nil {
		return nil, agents.ErrNotExist // a file, not a directory
	}
	out := make([]agents.Entry, 0, len(list))
	for _, e := range list {
		out = append(out, agents.Entry{Name: e.Name, Type: e.Type})
	}
	return out, nil
}

func (f configFiles) Read(ctx context.Context, path string) (string, error) {
	var out string
	status, err := f.g.do(ctx, "", http.MethodGet, fmt.Sprintf("/repos/%s/%s/raw/%s?ref=%s", f.ns, ConfigRepo, escapePath(path), url.QueryEscape(f.ref)), nil, &out)
	if status == http.StatusNotFound {
		return "", agents.ErrNotExist
	}
	return out, err
}

// Agents reads a namespace's agents from its config repo at ref.
func (g *Gitea) Agents(ctx context.Context, ns, ref string) ([]agents.Agent, []string, error) {
	return agents.Load(ctx, configFiles{g: g, ns: ns, ref: ref}, ns, ToolNames())
}
