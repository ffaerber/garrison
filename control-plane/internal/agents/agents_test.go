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

func TestNoFolderMeansTheOperator(t *testing.T) {
	got, problems, err := Load(context.Background(), mapFiles{"members.yaml": "admins: [alice]\n"}, "homepage", tools)
	if err != nil || len(problems) > 0 || len(got) != 1 {
		t.Fatalf("got %v %v %v", got, problems, err)
	}
	if got[0].ID() != "homepage/operator" || !slices.Equal(got[0].Tools, tools) || got[0].Prompt != OperatorPrompt {
		t.Errorf("operator = %+v", got[0])
	}
}

func TestTheWrittenOperatorLoads(t *testing.T) {
	files := mapFiles{}
	for p, c := range OperatorFiles() {
		files[p] = c
	}
	got, problems, err := Load(context.Background(), files, "homepage", tools)
	if err != nil || len(problems) > 0 || len(got) != 1 {
		t.Fatalf("got %v %v %v", got, problems, err)
	}
	if want := Operator("homepage", tools); got[0].Prompt != want.Prompt || !slices.Equal(got[0].Tools, want.Tools) {
		t.Errorf("written operator %+v differs from the built-in one", got[0])
	}
}

func TestTheCommanderLoadsInSystem(t *testing.T) {
	files := mapFiles{}
	for p, c := range OperatorFiles() {
		files[p] = c
	}
	for p, c := range CommanderFiles() {
		files[p] = c
	}
	got, problems, err := Load(context.Background(), files, SystemNamespace, tools)
	if err != nil || len(problems) > 0 || len(got) != 2 {
		t.Fatalf("got %v %v %v", got, problems, err)
	}
	i := slices.IndexFunc(got, func(a Agent) bool { return a.Name == CommanderName })
	if i < 0 {
		t.Fatalf("no commander in %+v", got)
	}
	c := got[i]
	if !c.AsksAny() || len(c.Tools) != 0 || c.Prompt != CommanderPrompt || c.Description == "" {
		t.Errorf("commander = %+v", c)
	}
	if !c.MayAsk("todo/developer") || c.MayAsk("system/commander") {
		t.Errorf("the commander may ask others, never itself")
	}
	if !strings.Contains(files["agents/commander/agent.yaml"], "agents: any") {
		t.Errorf("agent.yaml: %s", files["agents/commander/agent.yaml"])
	}
}

func TestAnyIsForSystemOnly(t *testing.T) {
	for _, c := range []struct {
		ns, yaml, want string
	}{
		{"todo", "agents: any\n", "only for the agents of system"},
		{"todo", "agents: [any]\n", "only for the agents of system"},
		{SystemNamespace, "agents: [any, todo/developer]\n", "stands alone"},
		{SystemNamespace, "agents: any\n", ""},
	} {
		files := mapFiles{"agents/x/agent.yaml": c.yaml, "agents/x/prompt.md": "p"}
		got, problems, err := Load(context.Background(), files, c.ns, tools)
		if err != nil {
			t.Fatal(err)
		}
		if c.want == "" {
			if len(problems) > 0 || len(got) != 1 || !got[0].AsksAny() {
				t.Errorf("%s %q: %v %+v", c.ns, c.yaml, problems, got)
			}
			continue
		}
		if len(got) != 0 || !slices.ContainsFunc(problems, func(p string) bool { return strings.Contains(p, c.want) }) {
			t.Errorf("%s %q: problems %q, want %q", c.ns, c.yaml, problems, c.want)
		}
	}
}

func TestAgentsAndSkills(t *testing.T) {
	files := mapFiles{
		"skills/homepage-screenshots/SKILL.md":        skill("homepage-screenshots", "Take screenshots for the homepage."),
		"skills/homepage-screenshots/viewports.yaml":  "desktop: 1440x900\n",
		"skills/unused/SKILL.md":                      skill("unused", "Not named by anyone."),
		"agents/README.md":                            "# Agents\n",
		"agents/marketing/agent.yaml":                 "description: Media\nmodel: gpt-oss:120b\ntools: [read_file, propose_change]\nskills: [homepage-screenshots]\nrepos: [homepage]\n",
		"agents/marketing/prompt.md":                  "You make the homepage look good.\n",
		"agents/marketing/skills/thumbnail/SKILL.md":  skill("thumbnail", "Make a YouTube thumbnail."),
		"agents/marketing/skills/thumbnail/frame.svg": "<svg/>",
		"agents/reader/agent.yaml":                    "tools: [read_file]\n",
		"agents/reader/prompt.md":                     "You read.\n",
	}
	got, problems, err := Load(context.Background(), files, "homepage", tools)
	if err != nil || len(problems) > 0 {
		t.Fatalf("problems %v, err %v", problems, err)
	}
	if len(got) != 2 || got[0].ID() != "homepage/marketing" || got[1].ID() != "homepage/reader" {
		t.Fatalf("got %+v", got)
	}
	m := got[0]
	if m.Model != "gpt-oss:120b" || m.Description != "Media" || !slices.Equal(m.Repos, []string{"homepage"}) {
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
	for _, want := range []string{"You are homepage/marketing", `namespace "homepage"`, "You make the homepage look good.", "- thumbnail: Make a YouTube thumbnail.", "load_skill", "Rules of the platform"} {
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
		"bad repo":         {mapFiles{"agents/x/agent.yaml": "repos: [..]\n", "agents/x/prompt.md": "p"}, "not a repo name"},
		"no shared skill":  {mapFiles{"agents/x/agent.yaml": "skills: [nope]\n", "agents/x/prompt.md": "p"}, `no shared skill "nope"`},
		"skill no front":   {mapFiles{"agents/x/agent.yaml": "", "agents/x/prompt.md": "p", "agents/x/skills/s/SKILL.md": "# s\n"}, "frontmatter"},
		"skill wrong name": {mapFiles{"agents/x/agent.yaml": "", "agents/x/prompt.md": "p", "agents/x/skills/s/SKILL.md": skill("other", "d")}, "differs from its folder"},
		"skill no desc":    {mapFiles{"agents/x/agent.yaml": "", "agents/x/prompt.md": "p", "agents/x/skills/s/SKILL.md": "---\nname: s\n---\n"}, "description is missing"},
		"skill twice": {mapFiles{"skills/s/SKILL.md": skill("s", "d"), "agents/x/agent.yaml": "skills: [s]\n", "agents/x/prompt.md": "p",
			"agents/x/skills/s/SKILL.md": skill("s", "d")}, "both shared and its own"},
	}
	for name, c := range cases {
		c.files["agents/good/agent.yaml"] = "tools: [read_file]\n"
		c.files["agents/good/prompt.md"] = "Good.\n"
		got, problems, err := Load(context.Background(), c.files, "ns", tools)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if !slices.ContainsFunc(problems, func(p string) bool { return strings.Contains(p, c.want) }) {
			t.Errorf("%s: problems %q lack %q", name, problems, c.want)
		}
		if !slices.ContainsFunc(got, func(a Agent) bool { return a.Name == "good" }) || slices.ContainsFunc(got, func(a Agent) bool { return a.Name != "good" }) {
			t.Errorf("%s: agents %+v, want only the good one", name, got)
		}
	}
}

func TestAnEmptyFolderMeansNoAgents(t *testing.T) {
	got, problems, err := Load(context.Background(), mapFiles{"agents/README.md": "none here\n"}, "ns", tools)
	if err != nil || len(problems) > 0 || len(got) != 0 {
		t.Errorf("got %v %v %v", got, problems, err)
	}
}
