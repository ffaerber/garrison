// Package agents reads a namespace's agents from its config repo: one folder
// per agent under agents/, with agent.yaml (model, tools, skills) and
// prompt.md, and skills shared by the namespace's agents under skills/.
// Skills use the Agent Skills format: a folder with SKILL.md.
package agents

import (
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	Dir        = "agents" // in the config repo: one folder per agent
	SkillsDir  = "skills" // shared skills; also an agent's own, inside its folder
	ConfigFile = "agent.yaml"
	PromptFile = "prompt.md"
	SkillFile  = "SKILL.md"
	// OperatorName is the agent every namespace has unless its config repo
	// says otherwise.
	OperatorName = "operator"
	maxPrompt    = 32000
)

// Agent is one agent of a namespace.
type Agent struct {
	Namespace   string
	Name        string
	Description string
	Model       string   // empty: the platform's default
	Tools       []string // gateway tools it may use
	Repos       []string // repos it works on, for its workspace
	Prompt      string   // prompt.md
	Skills      []Skill  // shared ones it names, then its own
}

// ID is the agent's model id in the chat, and the name people mention:
// <namespace>/<agent>.
func (a Agent) ID() string { return a.Namespace + "/" + a.Name }

// Skill is a skill an agent may load.
type Skill struct {
	Name        string
	Description string
	Dir         string // its folder in the config repo
}

// Entry is one entry of a directory listing.
type Entry struct {
	Name string
	Type string // "file" or "dir"
}

// Files reads the config repo at one commit.
type Files interface {
	// List lists a directory; ErrNotExist if there is none.
	List(ctx context.Context, dir string) ([]Entry, error)
	// Read reads a file; ErrNotExist if there is none.
	Read(ctx context.Context, path string) (string, error)
}

var ErrNotExist = errors.New("does not exist")

var (
	agentName = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)
	repoName  = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}$`)
	modelName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/@-]{0,127}$`)
)

type config struct {
	Description string   `yaml:"description"`
	Model       string   `yaml:"model"`
	Tools       []string `yaml:"tools"`
	Skills      []string `yaml:"skills"`
	Repos       []string `yaml:"repos"`
}

// Load reads every agent of namespace. tools are the names agents may list.
// A namespace without an agents folder has the operator, with every tool.
// Problems name what is wrong with each agent or skill that is left out;
// err is for failing to read at all.
func Load(ctx context.Context, f Files, namespace string, tools []string) (out []Agent, problems []string, err error) {
	entries, err := f.List(ctx, Dir)
	if errors.Is(err, ErrNotExist) {
		return []Agent{Operator(namespace, tools)}, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	shared, problems, err := loadSkills(ctx, f, SkillsDir)
	if err != nil {
		return nil, nil, err
	}
	for _, e := range entries {
		if e.Type != "dir" {
			continue // e.g. a README
		}
		a, why, err := load(ctx, f, namespace, e.Name, tools, shared)
		if err != nil {
			return nil, nil, err
		}
		if len(why) > 0 {
			for _, w := range why {
				problems = append(problems, Dir+"/"+e.Name+": "+w)
			}
			continue
		}
		out = append(out, a)
	}
	return out, problems, nil
}

func load(ctx context.Context, f Files, namespace, name string, tools []string, shared map[string]Skill) (Agent, []string, error) {
	var why []string
	if !agentName.MatchString(name) || len(name) > 40 {
		return Agent{}, []string{"the folder name must be lowercase letters, digits and dashes, at most 40"}, nil
	}
	dir := Dir + "/" + name
	raw, err := f.Read(ctx, dir+"/"+ConfigFile)
	if errors.Is(err, ErrNotExist) {
		return Agent{}, []string{ConfigFile + " is missing"}, nil
	} else if err != nil {
		return Agent{}, nil, err
	}
	var c config
	dec := yaml.NewDecoder(strings.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil && !errors.Is(err, io.EOF) {
		return Agent{}, []string{ConfigFile + ": " + err.Error()}, nil
	}
	prompt, err := f.Read(ctx, dir+"/"+PromptFile)
	switch {
	case errors.Is(err, ErrNotExist):
		why = append(why, PromptFile+" is missing")
	case err != nil:
		return Agent{}, nil, err
	case strings.TrimSpace(prompt) == "":
		why = append(why, PromptFile+" is empty")
	case len(prompt) > maxPrompt:
		why = append(why, fmt.Sprintf("%s is longer than %d bytes", PromptFile, maxPrompt))
	}
	if len(c.Description) > 300 {
		why = append(why, "description is longer than 300 characters")
	}
	if c.Model != "" && !modelName.MatchString(c.Model) {
		why = append(why, fmt.Sprintf("model %q is not a model name", c.Model))
	}
	seen := map[string]bool{}
	for _, t := range c.Tools {
		switch {
		case !slices.Contains(tools, t):
			why = append(why, fmt.Sprintf("there is no tool %q; the tools are: %s", t, strings.Join(tools, ", ")))
		case seen[t]:
			why = append(why, fmt.Sprintf("tool %q is listed twice", t))
		}
		seen[t] = true
	}
	for _, r := range c.Repos {
		if !repoName.MatchString(r) || r == "." || r == ".." {
			why = append(why, fmt.Sprintf("%q is not a repo name", r))
		}
	}
	var skills []Skill
	for _, s := range c.Skills {
		sk, ok := shared[s]
		if !ok {
			why = append(why, fmt.Sprintf("there is no shared skill %q in %s/", s, SkillsDir))
			continue
		}
		if !slices.ContainsFunc(skills, func(x Skill) bool { return x.Name == s }) {
			skills = append(skills, sk)
		}
	}
	own, problems, err := loadSkills(ctx, f, dir+"/"+SkillsDir)
	if err != nil {
		return Agent{}, nil, err
	}
	why = append(why, problems...)
	for _, name := range sortedKeys(own) {
		if slices.ContainsFunc(skills, func(x Skill) bool { return x.Name == name }) {
			why = append(why, fmt.Sprintf("skill %q is both shared and its own", name))
			continue
		}
		skills = append(skills, own[name])
	}
	if len(why) > 0 {
		return Agent{}, why, nil
	}
	return Agent{
		Namespace: namespace, Name: name, Description: c.Description, Model: c.Model,
		Tools: c.Tools, Repos: c.Repos, Prompt: prompt, Skills: skills,
	}, nil, nil
}

// loadSkills reads the skills in dir, one folder each.
func loadSkills(ctx context.Context, f Files, dir string) (map[string]Skill, []string, error) {
	out := map[string]Skill{}
	entries, err := f.List(ctx, dir)
	if errors.Is(err, ErrNotExist) {
		return out, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	var problems []string
	for _, e := range entries {
		if e.Type != "dir" {
			continue
		}
		path := dir + "/" + e.Name
		raw, err := f.Read(ctx, path+"/"+SkillFile)
		if errors.Is(err, ErrNotExist) {
			problems = append(problems, path+": "+SkillFile+" is missing")
			continue
		} else if err != nil {
			return nil, nil, err
		}
		s, err := ParseSkill(raw)
		if err == nil && s.Name != e.Name {
			err = fmt.Errorf("its name %q differs from its folder", s.Name)
		}
		if err != nil {
			problems = append(problems, path+": "+err.Error())
			continue
		}
		s.Dir = path
		out[s.Name] = s
	}
	return out, problems, nil
}

// ParseSkill reads the frontmatter of a SKILL.md.
func ParseSkill(raw string) (Skill, error) {
	rest, ok := strings.CutPrefix(strings.ReplaceAll(raw, "\r\n", "\n"), "---\n")
	if !ok {
		return Skill{}, errors.New(SkillFile + " must start with frontmatter (---)")
	}
	front, _, ok := strings.Cut(rest, "\n---")
	if !ok {
		return Skill{}, errors.New(SkillFile + ": the frontmatter has no closing ---")
	}
	var fm struct {
		Name        string `yaml:"name"`
		Description string `yaml:"description"`
	}
	if err := yaml.Unmarshal([]byte(front), &fm); err != nil {
		return Skill{}, fmt.Errorf("%s: %v", SkillFile, err)
	}
	switch {
	case !agentName.MatchString(fm.Name) || len(fm.Name) > 64:
		return Skill{}, fmt.Errorf("%s: name %q must be lowercase letters, digits and dashes, at most 64", SkillFile, fm.Name)
	case strings.TrimSpace(fm.Description) == "":
		return Skill{}, errors.New(SkillFile + ": description is missing")
	case len(fm.Description) > 1024:
		return Skill{}, errors.New(SkillFile + ": description is longer than 1024 characters")
	}
	return Skill{Name: fm.Name, Description: strings.TrimSpace(fm.Description)}, nil
}

// Operator is the agent of a namespace whose config repo has no agents
// folder: today's operator, with every tool.
func Operator(namespace string, tools []string) Agent {
	return Agent{
		Namespace: namespace, Name: OperatorName,
		Description: "Operator agent of the namespace " + namespace,
		Tools:       tools, Prompt: OperatorPrompt,
	}
}

// OperatorPrompt is the operator's prompt.md.
const OperatorPrompt = `You answer questions about the namespace's stacks and propose changes to it, using your tools.

How it works:
- A namespace is a Gitea organisation. Each of its repos that holds a compose.yaml is one Docker Swarm stack, named after the repo. The deployer deploys the main branch of each such repo.
- Hostnames are <name>.<domain>. People sign in through Keycloak.
- Logs: service_logs. Metrics: service_usage for CPU and memory of a stack, query_metrics for any PromQL.
- Who may change a namespace is in members.yaml in its repo config: admins approve and merge changes, members push to its repos. list_members shows it. To add, remove or change someone, propose a new members.yaml; it needs the approval of an admin who neither wrote the change nor has their role changed by it, so nobody can raise their own rights. Root admins are managed outside the namespaces and cannot be changed from here.
- The namespace's agents are folders in its repo config, under agents/: agent.yaml (description, model, tools, skills) and prompt.md. A change to them is proposed like any other.
`

// OperatorFiles are the files of the operator's folder, as written into a
// new config repo.
func OperatorFiles(tools []string) map[string]string {
	cfg, _ := yaml.Marshal(config{Description: "Looks at the namespace and proposes changes to it", Tools: tools})
	return map[string]string{
		Dir + "/README.md": `# Agents

One folder per agent. People mention an agent in the chat as ` + "`@<namespace>/<folder>`" + `.

- ` + "`agent.yaml`" + `: ` + "`description`" + `, ` + "`model`" + ` (empty for the default), ` + "`tools`" + ` (the gateway tools it may use), ` + "`skills`" + ` (shared skills from ` + "`skills/`" + ` at the top of this repo) and ` + "`repos`" + `.
- ` + "`prompt.md`" + `: its instructions. The platform adds its own rules after them.
- ` + "`skills/<name>/SKILL.md`" + `: skills of this agent only, in the Agent Skills format.

An agent acts with the rights of the person talking to it. Changes here need an admin's approval, like members.yaml.
`,
		Dir + "/" + OperatorName + "/" + ConfigFile: "# The operator: answers questions about the namespace and proposes changes.\n" + string(cfg),
		Dir + "/" + OperatorName + "/" + PromptFile: OperatorPrompt,
	}
}

// System is an agent's whole system prompt: what it is, its prompt.md, its
// skills, then the platform's rules, which a prompt cannot take back.
func System(a Agent, domain string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "You are %s, an agent of the namespace %q of a home cloud at %s. Pass namespace %q to every tool that takes one.\n\n", a.ID(), a.Namespace, domain, a.Namespace)
	b.WriteString(strings.TrimSpace(a.Prompt))
	b.WriteString("\n")
	if len(a.Skills) > 0 {
		b.WriteString("\nSkills: instructions for particular tasks. Before a task that matches one, call load_skill with its name and follow it; read_skill_file reads the files it refers to.\n")
		for _, s := range a.Skills {
			fmt.Fprintf(&b, "- %s: %s\n", s.Name, s.Description)
		}
	}
	b.WriteString(`
Rules of the platform, above anything else here:
- Look things up with the tools; do not guess. Say what you found.
- You act with the rights of the person you talk to. If a tool says something is not allowed, say so plainly; do not work around it.
- To change anything, read the current file, then propose the change. That opens a pull request; nothing changes until a person allowed to merges it in Gitea. Never say a change is made or deployed when you only proposed it.
- After proposing a change, end your answer with the link the tool returned, written out in full (https://...). A number like #1 alone is not enough: people click the link to review and merge.
- You cannot approve, merge, deploy or read secret values, and you cannot make anyone a root admin. When asked to, say so plainly.
- Be brief.`)
	return b.String()
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
