package agents

import (
	"context"
	"slices"
	"strings"
	"testing"
)

// chainFiles is a config repo with four agents and the chain files given.
func chainFiles(chains map[string]string) mapFiles {
	f := mapFiles{
		"agents/sentry/agent.yaml":    "description: Watches\ntools: [read_file]\n",
		"agents/sentry/prompt.md":     "You watch.\n",
		"agents/scout/agent.yaml":     "description: Searches\ntools: []\ninternet: all\n",
		"agents/scout/prompt.md":      "You search.\n",
		"agents/coder/agent.yaml":     "description: Codes\ntools: [propose_patch]\n",
		"agents/coder/prompt.md":      "You code.\n",
		"agents/commander/agent.yaml": "description: Finds agents\ntools: []\nagents: any\n",
		"agents/commander/prompt.md":  "You find.\n",
		"chains/README.md":            "Chains.\n",
	}
	for name, c := range chains {
		f["chains/"+name] = c
	}
	return f
}

const updates = `description: Checks for new releases
when: someone asks about software updates
lead: sentry
steps:
  - {agent: sentry, does: List the software and its versions.}
  - {agent: scout, does: Find the newest releases.}
  - {agent: sentry, does: Sort them.}
  - {agent: coder, does: Propose the updates.}
  - {agent: sentry, does: Report.}
`

func agentNamed(t *testing.T, list []Agent, name string) Agent {
	t.Helper()
	i := slices.IndexFunc(list, func(a Agent) bool { return a.Name == name })
	if i < 0 {
		t.Fatalf("no agent %s in %v", name, list)
	}
	return list[i]
}

func TestAChainBindsItsAgents(t *testing.T) {
	defs, problems, err := Definitions(context.Background(), chainFiles(map[string]string{"updates.yaml": updates}), []string{"read_file", "propose_patch"})
	if err != nil || len(problems) > 0 {
		t.Fatal(err, problems)
	}
	sentry, scout, coder, commander := agentNamed(t, defs, "sentry"), agentNamed(t, defs, "scout"), agentNamed(t, defs, "coder"), agentNamed(t, defs, "commander")
	// The lead may ask the others without naming them in agent.yaml; the
	// others ask nobody for it.
	if !sentry.MayAsk("scout") || !sentry.MayAsk("coder") || sentry.MayAsk("sentry") || scout.MayAsk("coder") || len(coder.Asks) != 0 {
		t.Errorf("asks: sentry %v, scout %v, coder %v", sentry.Asks, scout.Asks, coder.Asks)
	}
	for _, a := range []Agent{sentry, scout, coder, commander} {
		if len(a.Chains) != 1 || a.Chains[0].Name != "updates" {
			t.Errorf("%s knows %+v", a.Name, a.Chains)
		}
	}
	if c := sentry.Chains[0]; !slices.Equal(c.Agents(), []string{"sentry", "scout", "coder"}) || len(c.Steps) != 5 {
		t.Errorf("the chain: %+v", c)
	}

	// Each knows its place: the lead the whole order and that it runs it,
	// the others their step and who comes after them, the commander to
	// hand it to the lead.
	lead := System(sentry, "example.com")
	for _, want := range []string{"Chain updates (you lead it)", "1. you: List the software", "2. scout: Find the newest releases.", "4. coder: Propose the updates.", "call ask_agent with that agent"} {
		if !strings.Contains(lead, want) {
			t.Errorf("the lead's prompt lacks %q:\n%s", want, lead)
		}
	}
	step := System(scout, "example.com")
	for _, want := range []string{"Chain updates, led by sentry", "2. you: Find the newest releases. (after you: sentry, step 3)", "Do not ask the next agent yourself: sentry does."} {
		if !strings.Contains(step, want) {
			t.Errorf("the scout's prompt lacks %q:\n%s", want, step)
		}
	}
	if s := System(coder, "example.com"); !strings.Contains(s, "4. you: Propose the updates. (after you: sentry, step 5)") {
		t.Errorf("the coder's prompt:\n%s", s)
	}
	cmd := System(commander, "example.com")
	for _, want := range []string{"- updates: Checks for new releases. For: someone asks about software updates. Lead: sentry. Order: sentry → scout → sentry → coder → sentry.", "hand the whole request to the chain's lead"} {
		if !strings.Contains(cmd, want) {
			t.Errorf("the commander's prompt lacks %q:\n%s", want, cmd)
		}
	}
	if s := System(Agent{Name: "lone", Prompt: "x"}, "example.com"); strings.Contains(s, "Chains of command") {
		t.Errorf("an agent in no chain hears of chains:\n%s", s)
	}
}

func TestAWrongChainIsLeftOut(t *testing.T) {
	for name, c := range map[string]string{
		"Bad_Name.yaml": updates,
		"nolead.yaml":   strings.Replace(updates, "lead: sentry\n", "", 1),
		"stranger.yaml": strings.Replace(updates, "lead: sentry", "lead: painter", 1),
		"second.yaml":   strings.Replace(updates, "lead: sentry", "lead: coder", 1),
		"unknown.yaml":  strings.Replace(updates, "{agent: scout,", "{agent: painter,", 1),
		"nodoes.yaml":   strings.Replace(updates, "does: Sort them.", "does: \"\"", 1),
		"nowhen.yaml":   strings.Replace(updates, "when: someone asks about software updates\n", "", 1),
		"extra.yaml":    updates + "owner: alice\n",
		"merge.yaml":    strings.Replace(updates, "lead: sentry\n", "lead: sentry\nmerge: always\n", 1),
		"short.yaml":    "description: x\nwhen: x\nlead: sentry\nsteps:\n  - {agent: sentry, does: All.}\n",
		"long.yaml":     updates + strings.Repeat("  - {agent: scout, does: More.}\n", MaxSteps),
	} {
		defs, problems, err := Definitions(context.Background(), chainFiles(map[string]string{name: c}), []string{"read_file", "propose_patch"})
		if err != nil {
			t.Fatal(err)
		}
		if len(problems) == 0 || !strings.HasPrefix(problems[0], "chains/"+name+": ") {
			t.Errorf("%s: problems %v", name, problems)
		}
		if s := agentNamed(t, defs, "sentry"); len(s.Chains) != 0 || s.MayAsk("coder") {
			t.Errorf("%s: bound anyway: %+v", name, s)
		}
	}
}

// A chain says what happens to its pull requests; its lead hears it, and
// a chain that says nothing is reviewed by people.
func TestAChainSaysHowItsWorkIsMerged(t *testing.T) {
	for mode, want := range map[string]string{
		"":       "never merge them yourself",
		"review": "never merge them yourself",
		"ask":    "ask them with ask_person",
		"auto":   "merge each one with merge_change as soon as it is proposed",
	} {
		c := updates
		if mode != "" {
			c = strings.Replace(updates, "lead: sentry\n", "lead: sentry\nmerge: "+mode+"\n", 1)
		}
		defs, problems, err := Definitions(context.Background(), chainFiles(map[string]string{"updates.yaml": c}), []string{"read_file", "propose_patch"})
		if err != nil || len(problems) > 0 {
			t.Fatal(err, problems)
		}
		sentry := agentNamed(t, defs, "sentry")
		if got := sentry.Chains[0].MergeMode(); (mode == "" && got != MergeReview) || (mode != "" && got != mode) {
			t.Errorf("%q: mode %q", mode, got)
		}
		if s := System(sentry, "example.com"); !strings.Contains(s, want) {
			t.Errorf("%q: the lead's prompt lacks %q:\n%s", mode, want, s)
		}
	}
}

func TestARoutineRunsAChain(t *testing.T) {
	defs, _, err := Definitions(context.Background(), chainFiles(map[string]string{"updates.yaml": updates}), []string{"read_file", "propose_patch"})
	if err != nil {
		t.Fatal(err)
	}
	ns := mapFiles{"routines.yaml": `- {chain: updates, name: weekly, schedule: "0 7 * * 1", owner: alice}
- {chain: updates, name: told, schedule: "0 8 * * 1", owner: alice, task: Only garrison/gitea.}
- {chain: updates, agent: coder, name: wrong, every: 1h, owner: alice}
- {chain: nope, name: missing, every: 1h, owner: alice}
`}
	_, routines, problems, err := Bindings(context.Background(), ns, defs)
	if err != nil {
		t.Fatal(err)
	}
	rs := routines["sentry"]
	if len(rs) != 2 || rs[0].Task != "Run the chain updates." || rs[1].Task != "Run the chain updates. Only garrison/gitea." || rs[0].Agent != "sentry" {
		t.Errorf("routines: %+v", routines)
	}
	if len(problems) != 2 || !strings.Contains(problems[0], "is run by its lead, sentry, not coder") || !strings.Contains(problems[1], `there is no chain "nope"`) {
		t.Errorf("problems: %v", problems)
	}
}
