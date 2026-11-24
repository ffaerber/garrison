package gateway

// An agent proposes a skill, for itself or for an agent it asks: what it
// learnt makes the next run better, as a pull request on garrison/config
// that a root admin reviews. It changes one SKILL.md in the agent's own
// skills folder and nothing else: never agent.yaml (tools, internet, model),
// a prompt, a grant or a chain. So an agent can make itself better at its
// job, not more powerful.

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	"garrison/control-plane/internal/agents"
)

const (
	// SkillBranch starts the branch of every skill an agent proposes:
	// skill/<agent>/<skill>-<ms>. The approval check holds such pull
	// requests to skill files.
	SkillBranch = "skill/"
	// MaxSkillSize is the most a proposed SKILL.md may hold.
	MaxSkillSize = 32 << 10
)

var skillName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// SkillPath is where an agent's own skill lives in garrison/config.
func SkillPath(agent, skill string) string {
	return agents.Dir + "/" + agent + "/" + agents.SkillsDir + "/" + skill + "/" + agents.SkillFile
}

// proposeSkill runs propose_skill for agent, working for p.
func (t *Tools) proposeSkill(ctx context.Context, p Person, agent *agents.Agent, a args) (string, error) {
	if agent == nil {
		return "", refused{"propose_skill is an agent's tool"}
	}
	name := agents.NameOf(a.Agent)
	if strings.TrimSpace(a.Agent) == "" {
		name = agent.Name
	}
	if name != agent.Name && !agent.MayAsk(agent.Unit+"/"+name) {
		return "", refused{fmt.Sprintf("you may propose skills only for yourself and the agents you ask (%s)", strings.Join(agent.Asks, ", "))}
	}
	if !skillName.MatchString(a.Skill) {
		return "", fmt.Errorf("%w: skill %q: lowercase letters, digits and dashes", ErrInvalid, a.Skill)
	}
	if len(a.Content) > MaxSkillSize {
		return "", fmt.Errorf("%w: the skill is %d KB; keep it under %d KB: instructions, not material", ErrInvalid, len(a.Content)>>10, MaxSkillSize>>10)
	}
	if strings.TrimSpace(a.Description) == "" {
		return "", fmt.Errorf("%w: say in description why: what went wrong without it, and what helped", ErrInvalid)
	}
	s, err := agents.ParseSkill(a.Content)
	if err != nil {
		return "", fmt.Errorf("%w: %v. A skill starts with ---, name: %s, description: when to load it, ---, then its instructions", ErrInvalid, err, a.Skill)
	}
	if s.Name != a.Skill {
		return "", fmt.Errorf("%w: its frontmatter names it %q, but the skill is %q", ErrInvalid, s.Name, a.Skill)
	}
	defs, err := t.Gitea.Definitions(ctx)
	if err != nil {
		return "", err
	}
	i := slices.IndexFunc(defs, func(d agents.Agent) bool { return d.Name == name })
	if i < 0 {
		return "", fmt.Errorf("%w: garrison has no agent %q", ErrInvalid, name)
	}
	path := SkillPath(name, a.Skill)
	for _, sk := range defs[i].Skills {
		if sk.Name == a.Skill && sk.Dir+"/"+agents.SkillFile != path {
			return "", fmt.Errorf("%w: %s already has the shared skill %q, which only root admins change; propose one with another name", ErrInvalid, name, a.Skill)
		}
	}
	title := strings.TrimSpace(a.Title)
	if title == "" {
		title = fmt.Sprintf("%s: the skill %s", name, a.Skill)
	}
	note := fmt.Sprintf("Proposed by the agent %s, working for %s, through propose_skill. It changes only the skill %s of %s, nothing of its tools, internet, model or grants. A root admin reviews it; it is never merged by itself. Read it as instructions %s will follow.",
		agent.ID(), p.asker(), a.Skill, name, name)
	branch := fmt.Sprintf("%s%s/%s-%d", SkillBranch, name, a.Skill, time.Now().UnixNano()/1e6)
	// The platform's account opens it: no agent and no person of a unit may
	// write to garrison/config, and the person asking did not write it.
	c, err := t.Gitea.Propose(ctx, "", PlatformUnit, ConfigRepo, path, a.Content, title, a.Description, branch, note)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("Opened change %s/%s#%d with the skill %s of %s. Nothing is changed yet: a root admin must review and merge it; after that, %s can load it.\n%s",
		c.Unit, c.Repo, c.Number, a.Skill, name, name, t.links(c)), nil
}

// skillOnly says why a pull request on garrison/config from a skill branch
// may not be merged: a file other than an agent's own SKILL.md, so that
// nothing but propose_skill's one file rides on it. "" if it may.
func skillOnly(files []string) string {
	for _, f := range files {
		parts := strings.Split(f, "/")
		if len(parts) != 5 || parts[0] != agents.Dir || parts[2] != agents.SkillsDir || parts[4] != agents.SkillFile {
			return "a skill branch changes only agents/<agent>/skills/<skill>/SKILL.md, not " + f
		}
	}
	return ""
}

// pullFiles are the files a pull request changes.
func (g *Gitea) pullFiles(ctx context.Context, ns, repo string, number int) ([]string, error) {
	var files []struct {
		Filename string `json:"filename"`
	}
	if _, err := g.do(ctx, "", http.MethodGet, fmt.Sprintf("/repos/%s/%s/pulls/%d/files?limit=100", ns, repo, number), nil, &files); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(files))
	for _, f := range files {
		out = append(out, f.Filename)
	}
	return out, nil
}
