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
	Description: "Lists the agents you may hand work to with ask_agent, for this person and here, as they work in your namespace: what each does, its tools, skills and rights there. Call it when you do not know which agent fits.",
	InputSchema: map[string]any{
		"type": "object",
		"properties": map[string]any{
			// Not "namespace": agents are told to pass their own to every
			// tool that takes one.
			"in_namespace": map[string]any{"type": "string", "description": "the agents as they work in another namespace, with its rights there; leave it out for the one you work in, which is what you usually want"},
		},
		"additionalProperties": false,
	},
}

// findAgents runs find_agents for the sandbox c: the agents its agent may
// ask that its person may use and that may answer in this chat.
func (s *Server) findAgents(ctx context.Context, c sandbox.Caller, raw json.RawMessage) (string, bool) {
	var in struct {
		Namespace string `json:"in_namespace"`
	}
	if len(raw) > 0 && string(raw) != "null" {
		if err := json.Unmarshal(raw, &in); err != nil {
			return "error: find_agents takes only in_namespace", true
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
		if info, err := s.Poster.Channel(ctx, channel); err == nil && info.Namespace != "" {
			fmt.Fprintf(&b, "This channel belongs to the namespace %s.\n\n", info.Namespace)
		}
	}
	ns := asker.Namespace
	if in.Namespace != "" {
		ns = in.Namespace
	}
	n := 0
	for _, a := range list {
		if a.Namespace != ns || !asker.MayAsk(a.ID()) || inChain(c.Run.Chain, a.Name) {
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
		if in.Namespace != "" {
			b.WriteString(" Try without in_namespace.")
		}
	} else if ns != asker.Namespace {
		fmt.Fprintf(&b, "\nTo ask one of these working in %s, name it as %s/<agent> in ask_agent.", ns, ns)
	}
	return strings.TrimSpace(b.String()), false
}

// inChain tells whether the agent name is part of a run already, in any
// namespace.
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
		fmt.Fprintf(b, "  may %s on %s/%s, for %s\n", g.Level, a.Namespace, where, grantFor(g.For))
	}
}

func grantFor(f string) string {
	if f == "" {
		return agents.ForMembers
	}
	return f
}
