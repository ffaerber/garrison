package agents

import (
	"context"
	"path"
	"slices"
	"strings"
	"testing"
)

// mapFiles is a config repo in memory: path -> content.
type mapFiles map[string]string

func (m mapFiles) List(_ context.Context, dir string) ([]Entry, error) {
	seen := map[string]string{}
	for p := range m {
		rest, ok := strings.CutPrefix(p, dir+"/")
		if !ok {
			continue
		}
		name, _, sub := strings.Cut(rest, "/")
		if sub {
			seen[name] = "dir"
		} else {
			seen[name] = "file"
		}
	}
	if len(seen) == 0 {
		return nil, ErrNotExist
	}
	var out []Entry
	for _, n := range sortedKeys(seen) {
		out = append(out, Entry{Name: n, Type: seen[n]})
	}
	return out, nil
}

func (m mapFiles) Read(_ context.Context, p string) (string, error) {
	if s, ok := m[path.Clean(p)]; ok {
		return s, nil
	}
	return "", ErrNotExist
}

var tools = []string{"list_repos", "read_file", "propose_change"}

func skill(name, desc string) string {
	return "---\nname: " + name + "\ndescription: " + desc + "\nlicense: MIT\n---\n# " + name + "\nDo it well.\n"
}

// platformOnly loads garrison's agents from files, as they work in garrison.
func platformOnly(t *testing.T, files mapFiles) ([]Agent, []string) {
	t.Helper()
	got, problems, err := Load(context.Background(), files, files, PlatformNamespace, tools)
	if err != nil {
		t.Fatal(err)
	}
	return got, problems
}

func TestNoFolderMeansTheOperator(t *testing.T) {
	got, problems := platformOnly(t, mapFiles{"llm/README.md": "models\n"})
	if len(problems) > 0 || len(got) != 1 {
		t.Fatalf("got %v %v", got, problems)
	}
	if got[0].ID() != "garrison/operator" || !slices.Equal(got[0].Tools, tools) || got[0].Prompt != OperatorPrompt {
		t.Errorf("operator = %+v", got[0])
	}
}

func TestTheWrittenAgentsLoad(t *testing.T) {
	files := mapFiles{}
	for p, c := range PlatformFiles() {
		files[p] = c
	}
	got, problems := platformOnly(t, files)
	if len(problems) > 0 || len(got) != 2 {
		t.Fatalf("got %v %v", got, problems)
	}
	i := slices.IndexFunc(got, func(a Agent) bool { return a.Name == OperatorName })
	if want := Operator(PlatformNamespace, tools); i < 0 || got[i].Prompt != want.Prompt || !slices.Equal(got[i].Tools, want.Tools) {
		t.Errorf("written operator differs from the built-in one: %+v", got)
	}
	// It knows how to turn a wish into garrison's parts.
	if !slices.ContainsFunc(got[i].Skills, func(s Skill) bool { return s.Name == "build" && strings.Contains(s.Description, "cron") }) {
		t.Errorf("operator skills = %+v", got[i].Skills)
	}
	i = slices.IndexFunc(got, func(a Agent) bool { return a.Name == CommanderName })
	if i < 0 {
		t.Fatalf("no commander in %+v", got)
	}
	c := got[i]
	if !c.AsksAny() || len(c.Tools) != 0 || c.Prompt != CommanderPrompt || c.Description == "" {
		t.Errorf("commander = %+v", c)
	}
	if !c.MayAsk("todo/developer") || !c.MayAsk("operator") || c.MayAsk("garrison/commander") || c.MayAsk("lab/commander") {
		t.Errorf("the commander may ask others, never itself")
	}
	if !strings.Contains(files["agents/commander/agent.yaml"], "agents: any") {
		t.Errorf("agent.yaml: %s", files["agents/commander/agent.yaml"])
	}
}

// garrison's agents work in every namespace, each with that namespace's
// grants and routines, and nothing of another's.
func TestTheAgentsWorkInEveryNamespace(t *testing.T) {
	platform := mapFiles{
		"agents/operator/agent.yaml": "tools: all\n", "agents/operator/prompt.md": "Operate.\n",
		"agents/media/agent.yaml": "tools: [read_file]\n", "agents/media/prompt.md": "Shoot.\n",
		"grants.yaml": "- {agent: operator, repo: control-plane, level: propose}\n",
	}
	lab := mapFiles{
		"members.yaml":  "admins: [alice]\n",
		"grants.yaml":   "- {agent: media, repo: example, branch: screenshots, level: autonomous, unattended: true}\n",
		"routines.yaml": "- {agent: media, name: daily, schedule: \"@daily\", owner: alice, task: Take a screenshot.}\n",
	}
	got, problems, err := Load(context.Background(), platform, lab, "lab", tools)
	if err != nil || len(problems) > 0 || len(got) != 2 {
		t.Fatalf("got %+v %v %v", got, problems, err)
	}
	for _, a := range got {
		if a.Namespace != "lab" || a.DisplayName() != a.Name {
			t.Errorf("%+v", a)
		}
		switch a.Name {
		case "media":
			if len(a.Grants) != 1 || a.Grants[0].Repo != "example" || len(a.Routines) != 1 || a.Routines[0].Name != "daily" || !a.NeedsAccount() {
				t.Errorf("media in lab: %+v", a)
			}
		case "operator":
			if len(a.Grants) != 0 || len(a.Routines) != 0 || a.NeedsAccount() {
				t.Errorf("the operator has garrison's grants in lab: %+v", a)
			}
		}
	}
	home, _, _ := Load(context.Background(), platform, platform, PlatformNamespace, tools)
	if i := slices.IndexFunc(home, func(a Agent) bool { return a.Name == "operator" }); i < 0 || len(home[i].Grants) != 1 {
		t.Errorf("garrison's own grants: %+v", home)
	}
	sys := System(got[0], "example.com")
	if !strings.Contains(sys, `working in the namespace "lab"`) || !strings.Contains(sys, `Pass namespace "lab"`) {
		t.Errorf("system prompt:\n%s", sys)
	}
	if sys := System(home[0], "example.com"); !strings.Contains(sys, "no namespace in particular") {
		t.Errorf("garrison's system prompt:\n%s", sys)
	}
}

// A namespace's own agents folder is refused: the agents are garrison's.
// garrison's problems are not the namespace's to fix.
func TestANamespaceHasNoAgentsOfItsOwn(t *testing.T) {
	platform := mapFiles{"agents/x/agent.yaml": "tools: [rm_rf]\n", "agents/x/prompt.md": "p", "agents/ok/agent.yaml": "", "agents/ok/prompt.md": "p"}
	lab := mapFiles{"agents/operator/agent.yaml": "tools: all\n", "agents/operator/prompt.md": "Mine.\n"}
	got, problems, err := Load(context.Background(), platform, lab, "lab", tools)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != "ok" || got[0].Prompt != "p" {
		t.Errorf("agents %+v", got)
	}
	if len(problems) != 1 || !strings.Contains(problems[0], "agents are defined in garrison/config only") {
		t.Errorf("problems %q", problems)
	}
}

func TestAnyStandsAlone(t *testing.T) {
	for _, c := range []struct {
		yaml, want string
	}{
		{"agents: [any, developer]\n", "stands alone"},
		{"agents: [todo/developer]\n", "the agents are all garrison's; name it developer"},
		{"agents: any\n", ""},
	} {
		got, problems := platformOnly(t, mapFiles{"agents/x/agent.yaml": c.yaml, "agents/x/prompt.md": "p"})
		if c.want == "" {
			if len(problems) > 0 || len(got) != 1 || !got[0].AsksAny() {
				t.Errorf("%q: %v %+v", c.yaml, problems, got)
			}
			continue
		}
		if len(got) != 0 || !slices.ContainsFunc(problems, func(p string) bool { return strings.Contains(p, c.want) }) {
			t.Errorf("%q: problems %q, want %q", c.yaml, problems, c.want)
		}
	}
}

func TestAgentsAndSkills(t *testing.T) {
	files := mapFiles{
		"skills/homepage-screenshots/SKILL.md":        skill("homepage-screenshots", "Take screenshots for the homepage."),
		"skills/homepage-screenshots/viewports.yaml":  "desktop: 1440x900\n",
		"skills/unused/SKILL.md":                      skill("unused", "Not named by anyone."),
		"agents/README.md":                            "# Agents\n",
		"agents/marketing/agent.yaml":                 "description: Media\nmodel: gpt-oss:120b\ntools: [read_file, propose_change]\nskills: [homepage-screenshots]\n",
		"agents/marketing/prompt.md":                  "You make the homepage look good.\n",
		"agents/marketing/skills/thumbnail/SKILL.md":  skill("thumbnail", "Make a YouTube thumbnail."),
		"agents/marketing/skills/thumbnail/frame.svg": "<svg/>",
		"agents/reader/agent.yaml":                    "tools: [read_file]\n",
		"agents/reader/prompt.md":                     "You read.\n",
	}
	got, problems, err := Load(context.Background(), files, mapFiles{}, "homepage", tools)
	if err != nil || len(problems) > 0 {
		t.Fatalf("problems %v, err %v", problems, err)
	}
	if len(got) != 2 || got[0].ID() != "homepage/marketing" || got[1].ID() != "homepage/reader" {
		t.Fatalf("got %+v", got)
	}
	m := got[0]
	if m.Model != "gpt-oss:120b" || m.Description != "Media" {
		t.Errorf("marketing = %+v", m)
	}
	if len(m.Skills) != 2 || m.Skills[0].Name != "homepage-screenshots" || m.Skills[0].Dir != "skills/homepage-screenshots" ||
		m.Skills[1].Name != "thumbnail" || m.Skills[1].Dir != "agents/marketing/skills/thumbnail" {
		t.Errorf("skills = %+v", m.Skills)
	}
	if len(got[1].Skills) != 0 {
		t.Errorf("reader got skills %+v", got[1].Skills)
	}
	sys := System(m, "example.com")
	for _, want := range []string{"You are marketing", `namespace "homepage"`, "You make the homepage look good.", "- thumbnail: Make a YouTube thumbnail.", "load_skill", "Rules of the platform"} {
		if !strings.Contains(sys, want) {
			t.Errorf("system prompt lacks %q:\n%s", want, sys)
		}
	}
	if strings.Contains(System(got[1], "example.com"), "load_skill") {
		t.Error("an agent without skills is told about load_skill")
	}
	// The platform's rules come last, after the agent's own prompt.
	if strings.Index(sys, "Rules of the platform") < strings.Index(sys, "You make the homepage") {
		t.Error("the platform's rules come before the agent's prompt")
	}
}

func TestBadAgentsAreLeftOut(t *testing.T) {
	cases := map[string]struct {
		files mapFiles
		want  string
	}{
		"unknown tool":     {mapFiles{"agents/x/agent.yaml": "tools: [rm_rf]\n", "agents/x/prompt.md": "p"}, `there is no tool "rm_rf"`},
		"unknown field":    {mapFiles{"agents/x/agent.yaml": "tool: [read_file]\n", "agents/x/prompt.md": "p"}, "field tool not found"},
		"no prompt":        {mapFiles{"agents/x/agent.yaml": "tools: []\n"}, "prompt.md is missing"},
		"empty prompt":     {mapFiles{"agents/x/agent.yaml": "tools: []\n", "agents/x/prompt.md": " \n"}, "prompt.md is empty"},
		"no agent.yaml":    {mapFiles{"agents/x/prompt.md": "p"}, "agent.yaml is missing"},
		"bad name":         {mapFiles{"agents/X_1/agent.yaml": "", "agents/X_1/prompt.md": "p"}, "folder name"},
		"tool twice":       {mapFiles{"agents/x/agent.yaml": "tools: [read_file, read_file]\n", "agents/x/prompt.md": "p"}, "listed twice"},
		"bad model":        {mapFiles{"agents/x/agent.yaml": "model: 'a b'\n", "agents/x/prompt.md": "p"}, "not a model name"},
		"repos":            {mapFiles{"agents/x/agent.yaml": "repos: [site]\n", "agents/x/prompt.md": "p"}, "repos is not part of an agent"},
		"grants":           {mapFiles{"agents/x/agent.yaml": "grants: [{repo: todo, level: propose}]\n", "agents/x/prompt.md": "p"}, "grants is not part of an agent: each namespace gives them in grants.yaml"},
		"routines":         {mapFiles{"agents/x/agent.yaml": "routines: []\n", "agents/x/prompt.md": "p"}, "routines is not part of an agent"},
		"no shared skill":  {mapFiles{"agents/x/agent.yaml": "skills: [nope]\n", "agents/x/prompt.md": "p"}, `no shared skill "nope"`},
		"skill no front":   {mapFiles{"agents/x/agent.yaml": "", "agents/x/prompt.md": "p", "agents/x/skills/s/SKILL.md": "# s\n"}, "frontmatter"},
		"skill wrong name": {mapFiles{"agents/x/agent.yaml": "", "agents/x/prompt.md": "p", "agents/x/skills/s/SKILL.md": skill("other", "d")}, "differs from its folder"},
		"skill no desc":    {mapFiles{"agents/x/agent.yaml": "", "agents/x/prompt.md": "p", "agents/x/skills/s/SKILL.md": "---\nname: s\n---\n"}, "description is missing"},
		"skill twice": {mapFiles{"skills/s/SKILL.md": skill("s", "d"), "agents/x/agent.yaml": "skills: [s]\n", "agents/x/prompt.md": "p",
			"agents/x/skills/s/SKILL.md": skill("s", "d")}, "both shared and its own"},
		"asks unknown": {mapFiles{"agents/x/agent.yaml": "agents: [nobody]\n", "agents/x/prompt.md": "p"}, "there is no agent nobody"},
		"asks itself":  {mapFiles{"agents/x/agent.yaml": "agents: [garrison/x]\n", "agents/x/prompt.md": "p"}, "does not ask itself"},
	}
	for name, c := range cases {
		c.files["agents/good/agent.yaml"] = "tools: [read_file]\n"
		c.files["agents/good/prompt.md"] = "Good.\n"
		got, problems := platformOnly(t, c.files)
		if !slices.ContainsFunc(problems, func(p string) bool { return strings.Contains(p, c.want) }) {
			t.Errorf("%s: problems %q lack %q", name, problems, c.want)
		}
		if !slices.ContainsFunc(got, func(a Agent) bool { return a.Name == "good" }) || slices.ContainsFunc(got, func(a Agent) bool { return a.Name != "good" }) {
			t.Errorf("%s: agents %+v, want only the good one", name, got)
		}
	}
}

func TestAnEmptyFolderMeansNoAgents(t *testing.T) {
	got, problems := platformOnly(t, mapFiles{"agents/README.md": "none here\n"})
	if len(problems) > 0 || len(got) != 0 {
		t.Errorf("got %v %v", got, problems)
	}
}
