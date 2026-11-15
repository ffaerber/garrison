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
	Description: "Lists the agents you may hand work to with ask_agent, for this person and here: what each does, its tools, skills, repos and rights. Call it when you do not know which agent fits.",
	InputSchema: map[string]any{
		"type": "object",
		"properties": map[string]any{
			// Not "namespace": agents are told to pass their own to every
			// tool that takes one, which would hide all the others.
			"in_namespace": map[string]any{"type": "string", "description": "only the agents of this namespace; leave it out to see all, which is what you usually want"},
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
	n := 0
	for _, a := range list {
		if !asker.MayAsk(a.ID()) || slices.Contains(c.Run.Chain, a.ID()) || (in.Namespace != "" && a.Namespace != in.Namespace) {
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
	}
	return strings.TrimSpace(b.String()), false
}

// describe writes what an agent is for, to choose it by.
func describe(b *strings.Builder, a agents.Agent) {
	desc := strings.TrimSpace(a.Description)
	if desc == "" {
		desc = "(no description)"
	}
	fmt.Fprintf(b, "- %s: %s\n", a.ID(), desc)
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
	if len(a.Repos) > 0 {
		fmt.Fprintf(b, "  works on: %s\n", strings.Join(a.Repos, ", "))
	}
	if len(a.Asks) > 0 {
		fmt.Fprintf(b, "  asks: %s\n", strings.Join(a.Asks, ", "))
	}
	for _, g := range a.Grants {
		where := g.Repo
		if g.Branch != "" {
			where += " into " + g.Branch
		}
		fmt.Fprintf(b, "  may %s on %s, for %s\n", g.Level, where, grantFor(g.For))
	}
}

func grantFor(f string) string {
	if f == "" {
		return agents.ForMembers
	}
	return f
}
