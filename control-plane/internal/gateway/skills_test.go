package gateway

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"garrison/control-plane/internal/agents"
)

// skillGitea is garrison/config with a scout that has a shared skill, and
// records what is written.
type skillGitea struct {
	mu     sync.Mutex
	sudo   []string
	writes []map[string]any
	pulls  []map[string]any
}

func (f *skillGitea) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := strings.TrimPrefix(r.URL.Path, "/api/v1")
	if r.Method != http.MethodGet {
		f.sudo = append(f.sudo, r.Header.Get("Sudo"))
	}
	files := map[string]string{
		"agents/scout/agent.yaml":  "description: Web\ntools: []\ninternet: all\nskills: [find]\n",
		"agents/scout/prompt.md":   "You search.\n",
		"agents/coder/agent.yaml":  "description: Code\ntools: [read_file, propose_skill]\nagents: [scout]\n",
		"agents/coder/prompt.md":   "You code.\n",
		"agents/sentry/agent.yaml": "description: Watch\ntools: [read_file]\n",
		"agents/sentry/prompt.md":  "You watch.\n",
		"skills/find/SKILL.md":     "---\nname: find\ndescription: Finding things.\n---\nLook.\n",
	}
	switch {
	case p == "/repos/garrison/config/contents/agents":
		io.WriteString(w, `[{"name":"scout","type":"dir"},{"name":"coder","type":"dir"},{"name":"sentry","type":"dir"}]`)
	case p == "/repos/garrison/config/contents/skills":
		io.WriteString(w, `[{"name":"find","type":"dir"}]`)
	case strings.HasPrefix(p, "/repos/garrison/config/raw/") && files[strings.TrimPrefix(p, "/repos/garrison/config/raw/")] != "":
		io.WriteString(w, files[strings.TrimPrefix(p, "/repos/garrison/config/raw/")])
	case strings.HasPrefix(p, "/repos/garrison/config/contents/agents/") && r.Method == http.MethodPost:
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		body["path"] = strings.TrimPrefix(p, "/repos/garrison/config/contents/")
		f.writes = append(f.writes, body)
		io.WriteString(w, "{}")
	case p == "/repos/garrison/config/pulls" && r.Method == http.MethodPost:
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		f.pulls = append(f.pulls, body)
		io.WriteString(w, `{"number":12,"html_url":"https://git.example.com/garrison/config/pulls/12"}`)
	default:
		http.NotFound(w, r)
	}
}

const germanSources = "---\nname: german-sources\ndescription: Searching for German news and prices.\n---\nSearch in German too, and try the sites' own search.\n"

func TestAgentsProposeSkills(t *testing.T) {
	fg := &skillGitea{}
	srv := httptest.NewServer(fg)
	defer srv.Close()
	tools := &Tools{Gitea: &Gitea{URL: srv.URL, User: "admin", Password: "pw"}, Dashboard: "https://home.test"}
	coder := &agents.Agent{Unit: "homepage", Name: "coder", Asks: []string{"scout"}, Tools: []string{"read_file", "propose_skill"}}
	alice := Person{Name: "alice"}
	propose := func(a args) (string, error) { return tools.proposeSkill(context.Background(), alice, coder, a) }

	out, err := propose(args{Agent: "scout", Skill: "german-sources", Content: germanSources, Description: "It came back empty twice on German topics."})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Opened change garrison/config#12 with the skill german-sources of scout") || !strings.Contains(out, "a root admin must review") {
		t.Errorf("answer: %s", out)
	}
	if len(fg.writes) != 1 || len(fg.pulls) != 1 {
		t.Fatalf("writes %v, pulls %v", fg.writes, fg.pulls)
	}
	wr, pr := fg.writes[0], fg.pulls[0]
	content, _ := base64.StdEncoding.DecodeString(wr["content"].(string))
	if wr["path"] != "agents/scout/skills/german-sources/SKILL.md" || string(content) != germanSources || !strings.HasPrefix(wr["new_branch"].(string), "skill/scout/german-sources-") {
		t.Errorf("the write: %v", wr)
	}
	// The platform's account writes it, never the person's or an agent's.
	for _, as := range fg.sudo {
		if as != "" {
			t.Errorf("written as %q", as)
		}
	}
	if body := pr["body"].(string); !strings.Contains(body, "It came back empty twice") || !strings.Contains(body, "Proposed by the agent homepage/coder, working for alice") {
		t.Errorf("the pull request's description: %s", body)
	}

	for why, a := range map[string]args{
		"you may propose skills only for yourself and the agents you ask": {Agent: "sentry", Skill: "x", Content: "---\nname: x\ndescription: y\n---\n", Description: "d"},
		"its frontmatter names it":                                        {Agent: "scout", Skill: "other", Content: germanSources, Description: "d"},
		"must start with frontmatter":                                     {Agent: "scout", Skill: "x", Content: "just text", Description: "d"},
		"already has the shared skill":                                    {Agent: "scout", Skill: "find", Content: "---\nname: find\ndescription: y\n---\n", Description: "d"},
		"say in description why":                                          {Agent: "scout", Skill: "german-sources", Content: germanSources},
		"lowercase letters":                                               {Agent: "scout", Skill: "../coder", Content: germanSources, Description: "d"},
		"keep it under":                                                   {Agent: "scout", Skill: "big", Content: "---\nname: big\ndescription: y\n---\n" + strings.Repeat("x", MaxSkillSize), Description: "d"},
		"garrison has no agent":                                           {Agent: "coder2", Skill: "x", Content: "---\nname: x\ndescription: y\n---\n", Description: "d"},
	} {
		if a.Agent == "coder2" {
			coder.Asks = append(coder.Asks, "coder2")
		}
		if _, err := propose(a); err == nil || !strings.Contains(err.Error(), why) {
			t.Errorf("%s: %v", why, err)
		}
	}
	// For itself, by default.
	fg.writes = nil
	if _, err := propose(args{Skill: "tests", Content: "---\nname: tests\ndescription: Running the tests.\n---\nRun them.\n", Description: "d"}); err != nil || len(fg.writes) != 1 || fg.writes[0]["path"] != "agents/coder/skills/tests/SKILL.md" {
		t.Errorf("its own: %v %v", err, fg.writes)
	}
	if _, err := tools.proposeSkill(context.Background(), alice, nil, args{Skill: "x"}); err == nil {
		t.Error("without an agent")
	}
}

// The approval check holds a skill branch to agents' own SKILL.md files.
func TestSkillOnly(t *testing.T) {
	if why := skillOnly([]string{"agents/scout/skills/a/SKILL.md", "agents/coder/skills/b/SKILL.md"}); why != "" {
		t.Error(why)
	}
	for _, f := range []string{"agents/scout/agent.yaml", "agents/scout/prompt.md", "skills/find/SKILL.md", "agents/scout/skills/a/run.sh", "chains/x.yaml", "agents/scout/skills/a/b/SKILL.md"} {
		if skillOnly([]string{"agents/scout/skills/a/SKILL.md", f}) == "" {
			t.Errorf("%s rides along", f)
		}
	}
}
