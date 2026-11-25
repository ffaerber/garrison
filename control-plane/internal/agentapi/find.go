package agentapi

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"garrison/control-plane/internal/agents"
	"garrison/control-plane/internal/sandbox"
)

// findSpec is the tool find_agents, for an agent that may ask others: whom
// it may hand work to, for this person, here.
var findSpec = toolSpec{
	Name:        "find_agents",
	Description: "Lists the agents you may hand work to with ask_agent, for this person and here, as they work in your unit: what each does, its tools, skills and rights there. Call it when you do not know which agent fits.",
	InputSchema: map[string]any{
		"type": "object",
		"properties": map[string]any{
			// Not "unit": agents are told to pass their own to every
			// tool that takes one.
			"in_unit": map[string]any{"type": "string", "description": "the agents as they work in another unit, with its rights there, and the link to its channel; leave it out for the one you work in, which is what you usually want"},
		},
		"additionalProperties": false,
	},
}

// findAgents runs find_agents for the sandbox c: the agents its agent may
// ask that its person may use and that may answer in this chat.
func (s *Server) findAgents(ctx context.Context, c sandbox.Caller, raw json.RawMessage) (string, bool) {
	var in struct {
		Unit string `json:"in_unit"`
	}
	if len(raw) > 0 && string(raw) != "null" {
		if err := json.Unmarshal(raw, &in); err != nil {
			return "error: find_agents takes only in_unit", true
		}
	}
	asker, ok, err := s.Agents.Get(ctx, c.Agent)
	if err != nil || !ok {
		return "error: cannot read your own agent folder", true
	}
	list, err := s.Agents.All(ctx)
	if err != nil {
		return "error: " + err.Error(), true
	}
	var b strings.Builder
	if channel, ok := strings.CutPrefix(c.Chat, "channel:"); ok && s.Poster != nil {
		if info, err := s.Poster.Channel(ctx, channel); err == nil && info.Unit != "" {
			fmt.Fprintf(&b, "This channel belongs to the unit %s.\n", info.Unit)
			s.writeRepos(ctx, &b, c.Person, info.Unit)
			b.WriteString("\n")
		}
	}
	ns := asker.Unit
	if in.Unit != "" {
		ns = in.Unit
	}
	n := 0
	for _, a := range list {
		if a.Unit != ns || !asker.MayAsk(a.ID()) || inChain(c.Run.Chain, a.Name) {
			continue
		}
		if why, err := s.mayAnswer(ctx, c, a); err != nil {
			return "error: " + err.Error(), true
		} else if why != "" {
			continue
		}
		n++
		describe(&b, a)
	}
	if n == 0 {
		b.WriteString("There is no agent you may ask for this person here.")
		if in.Unit != "" {
			b.WriteString(" Try without in_unit.")
		}
	} else if ns != asker.Unit {
		fmt.Fprintf(&b, "\nTo ask one of these working in %s, name it as %s/<agent> in ask_agent.", ns, ns)
	}
	// Where to send the person to talk to them.
	if in.Unit != "" && s.Poster != nil {
		if url, err := s.Poster.UnitChannel(ctx, ns); err == nil && url != "" {
			fmt.Fprintf(&b, "\nThe channel of %s, where people talk to its agents: %s", ns, url)
		}
	}
	return strings.TrimSpace(b.String()), false
}

// writeRepos writes the repos of the unit ns that person may see, so
// that an agent asked about "the app" knows whether there are several.
// Without Gitea, or when they cannot be read, it writes nothing.
func (s *Server) writeRepos(ctx context.Context, b *strings.Builder, person, ns string) {
	if s.Gitea == nil || person == "" {
		return
	}
	repos, err := s.Gitea.Repos(ctx, person, ns)
	if err != nil || len(repos) == 0 {
		return
	}
	var apps []string
	for _, r := range repos {
		if r.Archived || r.Name == "config" {
			continue
		}
		app := r.Name
		if d := strings.TrimSpace(r.Description); d != "" {
			app += " (" + d + ")"
		}
		apps = append(apps, app)
	}
	switch len(apps) {
	case 0:
		fmt.Fprintf(b, "It has no app repos yet, only %s/config.\n", ns)
	case 1:
		fmt.Fprintf(b, "Its one app repo is %s/%s; its stacks are in %s/config.\n", ns, apps[0], ns)
	default:
		fmt.Fprintf(b, "Its app repos are %s; its stacks are in %s/config. A request about \"the app\" that names none of them could mean any.\n", strings.Join(apps, ", "), ns)
	}
}

// inChain tells whether the agent name is part of a run already, in any
// unit.
func inChain(chain []string, name string) bool {
	return slices.ContainsFunc(chain, func(id string) bool { return agents.NameOf(id) == name })
}

// describe writes what an agent is for, to choose it by.
func describe(b *strings.Builder, a agents.Agent) {
	desc := strings.TrimSpace(a.Description)
	if desc == "" {
		desc = "(no description)"
	}
	fmt.Fprintf(b, "- %s: %s\n", a.Name, desc)
	if len(a.Tools) > 0 {
		fmt.Fprintf(b, "  tools: %s\n", strings.Join(a.Tools, ", "))
	}
	if len(a.Skills) > 0 {
		var skills []string
		for _, sk := range a.Skills {
			skills = append(skills, sk.Name+" ("+sk.Description+")")
		}
		fmt.Fprintf(b, "  skills: %s\n", strings.Join(skills, "; "))
	}
	if len(a.Asks) > 0 {
		fmt.Fprintf(b, "  asks: %s\n", strings.Join(a.Asks, ", "))
	}
	for _, c := range a.Chains {
		if c.Lead == a.Name {
			fmt.Fprintf(b, "  leads the chain %s: %s (for: %s); ask it to run the chain %s\n", c.Name, c.Description, c.When, c.Name)
		}
	}
	for _, g := range a.Grants {
		where := g.Repo
		if g.Branch != "" {
			where += " into " + g.Branch
		}
		fmt.Fprintf(b, "  may %s on %s/%s, for %s\n", g.Level, a.Unit, where, grantFor(g.For))
	}
}

func grantFor(f string) string {
	if f == "" {
		return agents.ForMembers
	}
	return f
}
