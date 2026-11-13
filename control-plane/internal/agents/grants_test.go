package agents

import (
	"context"
	"strings"
	"testing"
	"time"
)

func load1(t *testing.T, ns, yaml string, more ...string) (Agent, []string) {
	t.Helper()
	files := mapFiles{"agents/dev/agent.yaml": yaml, "agents/dev/prompt.md": "Code.\n"}
	for _, name := range more {
		files["agents/"+name+"/agent.yaml"] = "tools: []\n"
		files["agents/"+name+"/prompt.md"] = "Help.\n"
	}
	got, problems, err := Load(context.Background(), files, ns, tools)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range got {
		if a.Name == "dev" {
			return a, problems
		}
	}
	return Agent{}, problems
}

func TestVisibleDefaultsToMembersButSystemServesEveryone(t *testing.T) {
	if a, p := load1(t, "homepage", "tools: []\n"); a.Visible != VisibleMembers || len(p) > 0 {
		t.Errorf("homepage: %q %v", a.Visible, p)
	}
	if a, p := load1(t, "system", "tools: []\n"); a.Visible != VisibleEveryone || len(p) > 0 {
		t.Errorf("system: %q %v", a.Visible, p)
	}
	if a, _ := load1(t, "homepage", "visible: everyone\n"); a.Visible != VisibleEveryone {
		t.Errorf("everyone: %q", a.Visible)
	}
	if _, p := load1(t, "homepage", "visible: friends\n"); len(p) != 1 {
		t.Errorf("friends: %v", p)
	}
	if Operator("homepage", tools).Visible != VisibleMembers || Operator("system", tools).Visible != VisibleEveryone {
		t.Error("the operator's visibility")
	}
}

func TestGrants(t *testing.T) {
	a, p := load1(t, "homepage", `
grants:
  - {repo: todo, branch: dev, level: autonomous}
  - {repo: todo, branch: main, level: autonomous, for: admins, checks: [ci]}
  - {repo: todo, branch: release, level: merge}
  - {repo: todo, level: propose, for: everyone, unattended: true}
`)
	if len(p) > 0 || len(a.Grants) != 4 {
		t.Fatalf("%v %+v", p, a.Grants)
	}
	if a.Grants[0].For != ForMembers {
		t.Errorf("for defaults to members: %q", a.Grants[0].For)
	}
	if !a.NeedsAccount() || strings.Join(a.GrantedRepos(), ",") != "todo" || a.Branch() != "agent/dev/" {
		t.Errorf("account: %v %v %s", a.NeedsAccount(), a.GrantedRepos(), a.Branch())
	}
	for _, c := range []struct {
		branch, level, role string
		unattended          bool
		ok                  bool
	}{
		{"dev", LevelAutonomous, "member", false, true},
		{"dev", LevelMerge, "admin", false, true},  // autonomous includes merge
		{"dev", LevelAutonomous, "", false, false}, // outsiders never merge
		{"main", LevelAutonomous, "member", false, false},
		{"main", LevelAutonomous, "admin", false, true},
		{"release", LevelAutonomous, "admin", false, false}, // only merge there
		{"release", LevelMerge, "member", false, true},
		{"", LevelPropose, "", false, true},             // everyone may propose
		{"", LevelPropose, "", true, true},              // also unattended
		{"dev", LevelAutonomous, "member", true, false}, // that grant is not for unattended runs
	} {
		if _, ok := a.GrantFor("todo", c.branch, c.level, c.role, c.unattended); ok != c.ok {
			t.Errorf("%+v: %v", c, ok)
		}
	}
	if _, ok := a.GrantFor("quotes", "", LevelPropose, "admin", false); ok {
		t.Error("a repo without a grant")
	}
}

func TestGrantsThatAreRefused(t *testing.T) {
	for yaml, want := range map[string]string{
		"grants: [{repo: config, level: propose}]":                              "no agent may have a grant on config",
		"grants: [{repo: todo, branch: main, level: merge}]":                    "say level: autonomous",
		"grants: [{repo: todo, branch: dev, level: autonomous, for: everyone}]": "only for propose",
		"grants: [{repo: todo, level: propose, for: homepage/admins}]":          "groups of this namespace",
		"grants: [{repo: todo, level: autonomous}]":                             "needs the branch",
		"grants: [{repo: todo, branch: dev, level: propose}]":                   "propose takes no branch",
		"grants: [{repo: todo, level: own}]":                                    "level must be",
		"grants: [{repo: todo, level: propose, checks: [ci]}]":                  "checks are for merges",
	} {
		a, p := load1(t, "homepage", yaml)
		if a.Name != "" || !strings.Contains(strings.Join(p, "\n"), want) {
			t.Errorf("%s: %v", yaml, p)
		}
	}
}

func TestRoutines(t *testing.T) {
	a, p := load1(t, "homepage", `
routines:
  - {name: nightly, every: 24h, owner: alice, task: Update the dependencies.}
  - {name: on-main, on_push: {repo: todo, branch: main}, owner: bob, task: Check the change.}
`)
	if len(p) > 0 || len(a.Routines) != 2 || a.Routines[0].Every != 24*time.Hour || a.Routines[1].OnPush.Repo != "todo" {
		t.Fatalf("%v %+v", p, a.Routines)
	}
	if !a.NeedsAccount() {
		t.Error("routines need an account")
	}
	for yaml, want := range map[string]string{
		"routines: [{name: x, every: 1m, owner: alice, task: t}]":                                      "between",
		"routines: [{name: x, owner: alice, task: t}]":                                                 "when it runs is missing",
		"routines: [{name: x, every: 1h, task: t}]":                                                    "owner",
		"routines: [{name: x, every: 1h, owner: alice}]":                                               "task",
		"routines: [{name: x, every: 1h, owner: a, task: t}, {name: x, every: 2h, owner: a, task: t}]": "two routines",
		"routines: [{name: x, every: 1h, on_push: {repo: todo, branch: main}, owner: a, task: t}]":     "not both",
	} {
		if _, p := load1(t, "homepage", yaml); !strings.Contains(strings.Join(p, "\n"), want) {
			t.Errorf("%s: %v", yaml, p)
		}
	}
}

func TestAsks(t *testing.T) {
	a, p := load1(t, "homepage", "agents: [homepage/tester, shop/dev]\n", "tester")
	if len(p) > 0 || strings.Join(a.Asks, ",") != "homepage/tester,shop/dev" {
		t.Fatalf("%v %v", p, a.Asks)
	}
	if _, p := load1(t, "homepage", "agents: [homepage/tester]\n"); !strings.Contains(strings.Join(p, ""), "there is no agent homepage/tester") {
		t.Errorf("missing: %v", p)
	}
	if _, p := load1(t, "homepage", "agents: [homepage/dev]\n"); !strings.Contains(strings.Join(p, ""), "does not ask itself") {
		t.Errorf("itself: %v", p)
	}
	if _, p := load1(t, "homepage", "agents: [tester]\n"); !strings.Contains(strings.Join(p, ""), "not an agent") {
		t.Errorf("bare name: %v", p)
	}
}

func TestDisabledAgentsAreLeftOut(t *testing.T) {
	if a, p := load1(t, "homepage", "enabled: false\ngrants: [{repo: todo, level: propose}]\n"); a.Name != "" || len(p) > 0 {
		t.Errorf("%+v %v", a, p)
	}
}
