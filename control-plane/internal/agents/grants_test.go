package agents

import (
	"context"
	"strings"
	"testing"
	"time"
)

// load1 is the agent dev, defined by yaml in garrison (with the agents
// more), as it works in ns with grants.yaml and routines.yaml (bindings,
// file name -> content) there.
func load1(t *testing.T, ns, yaml string, bindings map[string]string, more ...string) (Agent, []string) {
	t.Helper()
	platform := mapFiles{"agents/dev/agent.yaml": yaml, "agents/dev/prompt.md": "Code.\n"}
	for _, name := range more {
		platform["agents/"+name+"/agent.yaml"] = "tools: []\n"
		platform["agents/"+name+"/prompt.md"] = "Help.\n"
	}
	files := mapFiles{}
	for k, v := range bindings {
		files[k] = v
	}
	got, problems, err := Load(context.Background(), platform, files, ns, tools)
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

func grants(yaml string) map[string]string   { return map[string]string{GrantsFile: yaml} }
func routines(yaml string) map[string]string { return map[string]string{RoutinesFile: yaml} }

func TestVisible(t *testing.T) {
	if a, p := load1(t, "homepage", "tools: []\n", nil); a.Visible != "" || len(p) > 0 {
		t.Errorf("default: %q %v", a.Visible, p)
	}
	if a, _ := load1(t, "homepage", "visible: members\n", nil); a.Visible != VisibleMembers {
		t.Errorf("members: %q", a.Visible)
	}
	for _, v := range []string{"everyone", "friends"} {
		if _, p := load1(t, PlatformNamespace, "visible: "+v+"\n", nil); len(p) != 1 {
			t.Errorf("%s: %v", v, p)
		}
	}
	a, _ := load1(t, "homepage", "tools: []\n", map[string]string{
		GrantsFile:   "- {agent: dev, repo: todo, level: propose}\n",
		RoutinesFile: "- {agent: dev, name: r, on_push: {repo: api, branch: main}, owner: bob, task: t}\n",
	})
	if strings.Join(a.WorksOn(), ",") != "api,todo" {
		t.Errorf("works on %v", a.WorksOn())
	}
}

func TestGrants(t *testing.T) {
	a, p := load1(t, "homepage", "", grants(`
- {agent: dev, repo: todo, branch: dev, level: autonomous}
- {agent: dev, repo: todo, branch: main, level: autonomous, for: admins, checks: [ci]}
- {agent: garrison/dev, repo: todo, branch: release, level: merge}
- {agent: dev, repo: todo, level: propose, for: everyone, unattended: true}
- {agent: other, repo: todo, level: propose}
`), "other")
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
		"- {agent: dev, repo: config, level: propose}":                              "no agent may have a grant on config",
		"- {agent: dev, repo: todo, branch: main, level: merge}":                    "say level: autonomous",
		"- {agent: dev, repo: todo, branch: dev, level: autonomous, for: everyone}": "only for propose",
		"- {agent: dev, repo: todo, level: propose, for: homepage/admins}":          "groups of this namespace",
		"- {agent: dev, repo: todo, level: autonomous}":                             "needs the branch",
		"- {agent: dev, repo: todo, branch: dev, level: propose}":                   "propose takes no branch",
		"- {agent: dev, repo: todo, level: own}":                                    "level must be",
		"- {agent: dev, repo: todo, level: propose, checks: [ci]}":                  "checks are for merges",
		"- {repo: todo, level: propose}":                                            "agent is missing",
		"- {agent: nobody, repo: todo, level: propose}":                             `there is no agent "nobody" in garrison`,
		"- {agent: dev, repo: todo, level: propose, extra: 1}":                      "field extra not found",
	} {
		a, p := load1(t, "homepage", "", grants(yaml))
		if len(a.Grants) != 0 || !strings.Contains(strings.Join(p, "\n"), want) {
			t.Errorf("%s: %v", yaml, p)
		}
	}
}

func TestRoutines(t *testing.T) {
	a, p := load1(t, "homepage", "", routines(`
- {agent: dev, name: nightly, every: 24h, owner: alice, task: Update the dependencies.}
- {agent: dev, name: on-main, on_push: {repo: todo, branch: main}, owner: bob, task: Check the change.}
- {agent: dev, name: morning, schedule: "30 6 * * mon-fri", owner: bob, task: Sum up yesterday.}
- {agent: dev, name: down, on_alert: {alertname: ServiceDown}, owner: bob, task: Find out why.}
- {agent: dev, name: any, on_alert: {}, owner: bob, task: Look.}
- {agent: other, name: nightly, every: 24h, owner: alice, task: Its own.}
`), "other")
	if len(p) > 0 || len(a.Routines) != 5 || a.Routines[3].OnAlert["alertname"] != "ServiceDown" || a.Routines[4].OnAlert == nil || a.Routines[0].Every != 24*time.Hour || a.Routines[1].OnPush.Repo != "todo" || a.Routines[2].Cron == nil {
		t.Fatalf("%v %+v", p, a.Routines)
	}
	if !a.NeedsAccount() {
		t.Error("routines need an account")
	}
	for yaml, want := range map[string]string{
		"- {agent: dev, name: x, every: 1m, owner: alice, task: t}":                                                  "between",
		"- {agent: dev, name: x, owner: alice, task: t}":                                                             "when it runs is missing",
		"- {agent: dev, name: x, every: 1h, task: t}":                                                                "owner",
		"- {agent: dev, name: x, every: 1h, owner: alice}":                                                           "task",
		"[{agent: dev, name: x, every: 1h, owner: a, task: t}, {agent: dev, name: x, every: 2h, owner: a, task: t}]": "two routines",
		"- {agent: dev, name: x, every: 1h, on_push: {repo: todo, branch: main}, owner: a, task: t}":                 "not several",
		`- {agent: dev, name: x, every: 1h, schedule: "@daily", owner: a, task: t}`:                                  "not several",
		`- {agent: dev, name: x, on_alert: {namespace: shop}, owner: a, task: t}`:                                    "always this namespace",
		`- {agent: dev, name: x, on_alert: {"a-b": c}, owner: a, task: t}`:                                           "not a label name",
		`- {agent: dev, name: x, schedule: "*/5 * * * *", owner: a, task: t}`:                                        "more often than every 10m",
		`- {agent: dev, name: x, schedule: "0 0 30 2 *", owner: a, task: t}`:                                         "never comes",
		`- {agent: dev, name: x, schedule: "at six", owner: a, task: t}`:                                             "want five fields",
		`- {name: x, every: 1h, owner: a, task: t}`:                                                                  "agent is missing",
	} {
		if _, p := load1(t, "homepage", "", routines(yaml)); !strings.Contains(strings.Join(p, "\n"), want) {
			t.Errorf("%s: %v", yaml, p)
		}
	}
}

func TestAsks(t *testing.T) {
	a, p := load1(t, "homepage", "agents: [tester, garrison/shop]\n", nil, "tester", "shop")
	if len(p) > 0 || strings.Join(a.Asks, ",") != "tester,shop" {
		t.Fatalf("%v %v", p, a.Asks)
	}
	if !a.MayAsk("tester") || !a.MayAsk("homepage/tester") || !a.MayAsk("lab/shop") || a.MayAsk("other") {
		t.Errorf("may ask: %v", a.Asks)
	}
	if _, p := load1(t, PlatformNamespace, "agents: [tester]\n", nil); !strings.Contains(strings.Join(p, ""), "there is no agent tester") {
		t.Errorf("missing: %v", p)
	}
	if _, p := load1(t, PlatformNamespace, "agents: [dev]\n", nil); !strings.Contains(strings.Join(p, ""), "does not ask itself") {
		t.Errorf("itself: %v", p)
	}
}

func TestDisabledAgentsAreLeftOut(t *testing.T) {
	if a, p := load1(t, "homepage", "enabled: false\n", grants("- {agent: dev, repo: todo, level: propose}\n")); a.Name != "" || !strings.Contains(strings.Join(p, ""), `there is no agent "dev"`) {
		t.Errorf("%+v %v", a, p)
	}
}
