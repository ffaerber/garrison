package agents

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// ChainsDir holds the chains of command in garrison's config repo, one file
// each: chains/<name>.yaml.
const ChainsDir = "chains"

// MaxSteps is how many steps a chain may have.
const MaxSteps = 12

// Chain is a chain of command: the order in which agents do one kind of
// job. Its lead runs it: it does its own steps and asks the others in
// order, one after another, each answering back to it.
type Chain struct {
	Name        string `yaml:"-"`
	Description string `yaml:"description"`
	When        string `yaml:"when"` // which requests it is for, in words
	Lead        string `yaml:"lead"`
	Steps       []Step `yaml:"steps"`
}

// Step is one step of a chain: an agent and what it does there.
type Step struct {
	Agent string `yaml:"agent"`
	Does  string `yaml:"does"`
}

// Agents are the chain's agents, each once, in the order they first come.
func (c Chain) Agents() []string {
	var out []string
	for _, s := range c.Steps {
		if !slices.Contains(out, s.Agent) {
			out = append(out, s.Agent)
		}
	}
	return out
}

// Has tells whether the agent name has a step in c.
func (c Chain) Has(name string) bool { return slices.Contains(c.Agents(), name) }

// LoadChains reads the chains of garrison's config repo (f) for the agents
// defs. A chain that is wrong is left out and named in problems.
func LoadChains(ctx context.Context, f Files, defs []Agent) ([]Chain, []string, error) {
	entries, err := f.List(ctx, ChainsDir)
	if errors.Is(err, ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	var out []Chain
	var problems []string
	for _, e := range entries {
		name, ok := strings.CutSuffix(e.Name, ".yaml")
		if e.Type != "file" || !ok {
			continue // e.g. a README
		}
		at := ChainsDir + "/" + e.Name
		raw, err := f.Read(ctx, at)
		if err != nil {
			return nil, nil, err
		}
		c, why := parseChain(name, raw, defs)
		if len(why) > 0 {
			for _, w := range why {
				problems = append(problems, at+": "+w)
			}
			continue
		}
		out = append(out, c)
	}
	return out, problems, nil
}

func parseChain(name, raw string, defs []Agent) (Chain, []string) {
	var c Chain
	if !agentName.MatchString(name) || len(name) > 40 {
		return c, []string{"the file name must be lowercase letters, digits and dashes, at most 40, then .yaml"}
	}
	if err := decodeStrict(raw, &c); err != nil {
		return c, []string{err.Error()}
	}
	c.Name = name
	var why []string
	known := func(agent string) bool {
		return slices.ContainsFunc(defs, func(a Agent) bool { return a.Name == agent })
	}
	switch {
	case strings.TrimSpace(c.Description) == "" || len(c.Description) > 300:
		why = append(why, "description is needed, at most 300 characters")
	}
	if strings.TrimSpace(c.When) == "" || len(c.When) > 300 {
		why = append(why, "when is needed: which requests the chain is for, at most 300 characters")
	}
	switch {
	case len(c.Steps) < 2:
		why = append(why, "a chain needs at least two steps")
	case len(c.Steps) > MaxSteps:
		why = append(why, fmt.Sprintf("a chain has at most %d steps", MaxSteps))
	}
	switch {
	case c.Lead == "":
		why = append(why, "lead is missing: the agent that runs the chain")
	case !known(c.Lead):
		why = append(why, fmt.Sprintf("lead: there is no agent %q", c.Lead))
	case len(c.Steps) > 0 && c.Steps[0].Agent != c.Lead:
		why = append(why, fmt.Sprintf("the first step is the lead's, %s: it starts the chain", c.Lead))
	}
	for i, s := range c.Steps {
		switch {
		case s.Agent == "":
			why = append(why, fmt.Sprintf("steps[%d]: agent is missing", i))
		case !known(s.Agent):
			why = append(why, fmt.Sprintf("steps[%d]: there is no agent %q", i, s.Agent))
		}
		if strings.TrimSpace(s.Does) == "" || len(s.Does) > 1000 {
			why = append(why, fmt.Sprintf("steps[%d]: does is needed, at most 1000 characters", i))
		}
	}
	if len(why) > 0 {
		return Chain{}, why
	}
	return c, nil
}

// bindChains gives each agent the chains it is part of, and the commander
// (any agent that may ask any) every chain. A lead may ask the agents of
// its chains.
func bindChains(defs []Agent, chains []Chain) {
	for i := range defs {
		a := &defs[i]
		for _, c := range chains {
			if a.AsksAny() || c.Has(a.Name) {
				a.Chains = append(a.Chains, c)
			}
			if c.Lead != a.Name || a.AsksAny() {
				continue
			}
			for _, other := range c.Agents() {
				if other != a.Name && !slices.Contains(a.Asks, other) {
					a.Asks = append(a.Asks, other)
				}
			}
		}
	}
}

// ChainNamed is the chain name among those a knows, if any.
func ChainNamed(defs []Agent, name string) (Chain, bool) {
	for _, a := range defs {
		for _, c := range a.Chains {
			if c.Name == name {
				return c, true
			}
		}
	}
	return Chain{}, false
}

// chainsPrompt is the part of an agent's system prompt about the chains it
// knows: the whole order for the lead, its place and who comes next for an
// agent in one, and every chain in short for the commander.
func chainsPrompt(a Agent) string {
	if len(a.Chains) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\nChains of command: the order in which agents do a kind of job. Its lead runs it: it does its own steps and asks the others in order with ask_agent, one after another; each answers back to the lead.\n")
	for _, c := range a.Chains {
		switch {
		case c.Lead == a.Name:
			fmt.Fprintf(&b, "\nChain %s (you lead it): %s. For: %s.\n", c.Name, c.Description, c.When)
			for i, s := range c.Steps {
				who := s.Agent
				if who == a.Name {
					who = "you"
				}
				fmt.Fprintf(&b, "%d. %s: %s\n", i+1, who, s.Does)
			}
			b.WriteString("When a request is for this chain, run it: do your steps yourself; for each step of another agent, call ask_agent with that agent, the step as task, and what it needs from the steps before (only that: never secrets, tokens or private data to an agent that reaches the internet). Wait for its answer before the next step. If a step fails, stop there and report what was done and what failed. End with the report the last step asks for, with every link the others gave.\n")
		case c.Has(a.Name):
			fmt.Fprintf(&b, "\nChain %s, led by %s: %s.\n", c.Name, c.Lead, c.Description)
			for i, s := range c.Steps {
				who := s.Agent
				if who == a.Name {
					who = "you"
				}
				next := ""
				if s.Agent == a.Name && i+1 < len(c.Steps) {
					next = fmt.Sprintf(" (after you: %s, step %d)", c.Steps[i+1].Agent, i+2)
				}
				fmt.Fprintf(&b, "%d. %s: %s%s\n", i+1, who, s.Does, next)
			}
			fmt.Fprintf(&b, "When %s asks you for a step of this chain, do that step only, and answer %s with what the next step needs. Do not ask the next agent yourself: %s does.\n", c.Lead, c.Lead, c.Lead)
		default:
			var order []string
			for _, s := range c.Steps {
				order = append(order, s.Agent)
			}
			fmt.Fprintf(&b, "- %s: %s. For: %s. Lead: %s. Order: %s.\n", c.Name, c.Description, c.When, c.Lead, strings.Join(order, " → "))
		}
	}
	if a.AsksAny() {
		b.WriteString("When a request matches a chain's For, hand the whole request to the chain's lead with ask_agent, naming the chain in the task (\"Run the chain <name>: ...\"); do not split it among the agents yourself.\n")
	}
	return b.String()
}
