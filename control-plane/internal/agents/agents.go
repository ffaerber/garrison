// Package agents reads the agents. They are all defined in garrison's
// config repo: one folder per agent under agents/, with agent.yaml (model,
// tools, skills) and prompt.md, and skills shared by them under skills/.
// Skills use the Agent Skills format: a folder with SKILL.md.
//
// Each agent works in every unit: in a unit it is an instance,
// <unit>/<agent>, with the rights (grants.yaml) and routines
// (routines.yaml) that unit's config repo gives it.
package agents

import (
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"
	"time"

	"garrison/templates"

	"gopkg.in/yaml.v3"
)

const (
	Dir        = "agents" // in the config repo: one folder per agent
	SkillsDir  = "skills" // shared skills; also an agent's own, inside its folder
	ConfigFile = "agent.yaml"
	PromptFile = "prompt.md"
	SkillFile  = "SKILL.md"
	// OperatorName is the agent garrison has unless its config repo says
	// otherwise.
	OperatorName = "operator"
	// GrantsFile and RoutinesFile, in a unit's config repo, give
	// garrison's agents rights and routines there.
	GrantsFile   = "grants.yaml"
	RoutinesFile = "routines.yaml"
	// CommanderName is the agent of garrison that finds the agent for a task.
	CommanderName = "commander"
	// PlatformUnit is the platform's own unit.
	PlatformUnit = "garrison"
	// AnyAgent, as agents: any, lets an agent ask every agent the person may
	// use. Only garrison's agents, which root admins approve, may.
	AnyAgent  = "any"
	maxPrompt = 32000
)

// Agent is one of garrison's agents, working in a unit: its instance
// there.
type Agent struct {
	Unit        string // where it works: its rights, routines and channel
	Name        string
	Description string
	Model       string   // empty: the platform's default
	Tools       []string // gateway tools it may use
	Prompt      string   // prompt.md
	Skills      []Skill  // shared ones it names, then its own
	Visible     string   // members: only the unit's people; empty: as its repos (WorksOn)
	Asks        []string // agents it may ask for work, by name
	Grants      []Grant  // rights of its own in Unit (grants.yaml)
	Routines    []Routine
	Internet    Internet // what its sandbox reaches through the proxy
	Chains      []Chain  // the chains of command it is part of; every one for the commander
	// Turn is how long one of its turns may take; 0: the agent service's
	// default (10 minutes). For agents whose work takes long, e.g. designer,
	// whose videos take minutes each.
	Turn time.Duration
	// Memory is its sandbox's memory in bytes; 0: the agent service's
	// default (1 GiB). For agents whose tools need more, e.g. designer's ffmpeg.
	Memory int64
}

// MaxMemory is the most memory an agent.yaml may give its sandbox: the
// most the admission policy garrison-sandboxes lets a sandbox have
// (garrison/platform/kubernetes.yaml).
const MaxMemory = 4 << 30

// MaxTurn is the longest turn an agent.yaml may give its agent.
const MaxTurn = 30 * time.Minute

// ID is the instance: <unit>/<agent>. Garrison's, garrison/<agent>, is
// the model id in the chat.
func (a Agent) ID() string { return a.Unit + "/" + a.Name }

// DisplayName is what people see and type after @ in the chat: the agent's
// name. It is the same agent in every channel, working in the channel's
// unit.
func (a Agent) DisplayName() string { return a.Name }

// NameOf is the agent's name in an ID or a name: operator for
// lab/operator, garrison/operator or operator.
func NameOf(id string) string {
	_, name, ok := strings.Cut(id, "/")
	if !ok {
		return id
	}
	return name
}

// AsksAny tells whether a may ask every agent the person may use.
func (a Agent) AsksAny() bool { return slices.Contains(a.Asks, AnyAgent) }

// MayAsk tells whether a may hand work to the agent id (an instance or a
// name): one of its agents, or any but itself with agents: any. Whether the
// person may use it is the agent service's to check.
func (a Agent) MayAsk(id string) bool {
	name := NameOf(id)
	return name != a.Name && (a.AsksAny() || slices.ContainsFunc(a.Asks, func(x string) bool { return NameOf(x) == name }))
}

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
	Tools       toolList `yaml:"tools"`
	Skills      []string `yaml:"skills"`
	Visible     string   `yaml:"visible,omitempty"` // only "members"
	Agents      askList  `yaml:"agents,omitempty"`
	Internet    Internet `yaml:"internet,omitempty"`
	Enabled     *bool    `yaml:"enabled,omitempty"`
	Turn        string   `yaml:"turn,omitempty"`   // e.g. 30m: how long a turn may take, at most MaxTurn
	Memory      string   `yaml:"memory,omitempty"` // e.g. 4Gi: its sandbox's memory, 256Mi to MaxMemory
	// Not here any more: each unit gives the agent these in its own
	// config repo, or none.
	Repos    any `yaml:"repos,omitempty"`
	Grants   any `yaml:"grants,omitempty"`
	Routines any `yaml:"routines,omitempty"`
}

// AllTools, as tools: all, gives an agent every tool, also those added
// later.
const AllTools = "all"

// toolList is tools in agent.yaml: a list of tools, or all.
type toolList []string

func (l *toolList) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode && n.Value == AllTools {
		*l = toolList{AllTools}
		return nil
	}
	var names []string
	if err := n.Decode(&names); err != nil {
		return err
	}
	*l = names
	return nil
}

// askList is agents in agent.yaml: a list of agents, or any.
type askList []string

func (l *askList) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		*l = askList{n.Value}
		return nil
	}
	var ids []string
	if err := n.Decode(&ids); err != nil {
		return err
	}
	*l = ids
	return nil
}

func (l askList) MarshalYAML() (any, error) {
	if len(l) == 1 && l[0] == AnyAgent {
		return AnyAgent, nil
	}
	return []string(l), nil
}

// Load reads the agents as they work in unit: garrison's agents, from
// platform (garrison's config repo), with the grants and routines of f
// (unit's config repo; platform itself for garrison). tools are the
// names agents may list. Problems name what is wrong with each agent, skill,
// grant or routine that is left out; garrison's own problems only for
// garrison. err is for failing to read at all.
func Load(ctx context.Context, platform, f Files, unit string, tools []string) ([]Agent, []string, error) {
	defs, problems, err := Definitions(ctx, platform, tools)
	if err != nil {
		return nil, nil, err
	}
	if unit != PlatformUnit {
		problems = nil // garrison's to fix, in its own pull requests
		if _, err := f.List(ctx, Dir); err == nil {
			problems = append(problems, Dir+"/: agents are defined in garrison/config only, and work in every unit; remove this folder, and give them rights and routines here in "+GrantsFile+" and "+RoutinesFile)
		} else if !errors.Is(err, ErrNotExist) {
			return nil, nil, err
		}
	}
	grants, routines, more, err := Bindings(ctx, f, defs)
	if err != nil {
		return nil, nil, err
	}
	return Bind(defs, unit, grants, routines), append(problems, more...), nil
}

// Bind makes the instances of garrison's agents in unit, each with its
// grants and routines there.
func Bind(defs []Agent, unit string, grants map[string][]Grant, routines map[string][]Routine) []Agent {
	out := make([]Agent, 0, len(defs))
	for _, d := range defs {
		d.Unit = unit
		d.Grants = grants[d.Name]
		d.Routines = routines[d.Name]
		out = append(out, d)
	}
	return out
}

// Bindings reads a unit's grants.yaml and routines.yaml: lists, each
// entry naming one of garrison's agents (defs) with agent. Entries that are
// wrong are left out, and named in problems.
func Bindings(ctx context.Context, f Files, defs []Agent) (map[string][]Grant, map[string][]Routine, []string, error) {
	known := func(name string) string {
		switch {
		case name == "":
			return "agent is missing: which of garrison's agents it is for"
		case !slices.ContainsFunc(defs, func(a Agent) bool { return a.Name == name }):
			var names []string
			for _, a := range defs {
				names = append(names, a.Name)
			}
			return fmt.Sprintf("there is no agent %q in garrison; the agents are: %s", name, strings.Join(names, ", "))
		}
		return ""
	}
	var problems []string
	grants := map[string][]Grant{}
	var gs []Grant
	if raw, err := f.Read(ctx, GrantsFile); err == nil {
		if err := decodeStrict(raw, &gs); err != nil {
			problems = append(problems, GrantsFile+": "+err.Error())
			gs = nil
		}
	} else if !errors.Is(err, ErrNotExist) {
		return nil, nil, nil, err
	}
	for i, g := range gs {
		at := fmt.Sprintf("%s: [%d]", GrantsFile, i)
		g.Agent = NameOf(g.Agent)
		if why := known(g.Agent); why != "" {
			problems = append(problems, at+": "+why)
			continue
		}
		ok, why := checkGrants([]Grant{g})
		if len(why) > 0 {
			for _, w := range why {
				problems = append(problems, at+" "+strings.TrimPrefix(w, "grants[0]: "))
			}
			continue
		}
		grants[g.Agent] = append(grants[g.Agent], ok...)
	}
	routines := map[string][]Routine{}
	var rs []Routine
	if raw, err := f.Read(ctx, RoutinesFile); err == nil {
		if err := decodeStrict(raw, &rs); err != nil {
			problems = append(problems, RoutinesFile+": "+err.Error())
			rs = nil
		}
	} else if !errors.Is(err, ErrNotExist) {
		return nil, nil, nil, err
	}
	seen := map[string]bool{}
	for i, r := range rs {
		at := fmt.Sprintf("%s: [%d]", RoutinesFile, i)
		r.Agent = NameOf(r.Agent)
		if r.Chain != "" {
			c, ok := ChainNamed(defs, r.Chain)
			switch {
			case !ok:
				problems = append(problems, fmt.Sprintf("%s: there is no chain %q in garrison's %s/", at, r.Chain, ChainsDir))
				continue
			case r.Agent == "":
				r.Agent = c.Lead
			case r.Agent != c.Lead:
				problems = append(problems, fmt.Sprintf("%s: the chain %s is run by its lead, %s, not %s; leave agent out", at, c.Name, c.Lead, r.Agent))
				continue
			}
			r.Task = strings.TrimSpace("Run the chain " + c.Name + ". " + r.Task)
		}
		if why := known(r.Agent); why != "" {
			problems = append(problems, at+": "+why)
			continue
		}
		ok, why := checkRoutines([]Routine{r})
		if seen[r.Agent+"/"+r.Name] {
			why = append(why, fmt.Sprintf("there are two routines %q of %s", r.Name, r.Agent))
		}
		seen[r.Agent+"/"+r.Name] = true
		if len(why) > 0 {
			for _, w := range why {
				problems = append(problems, at+" "+strings.TrimPrefix(w, "routines[0]: "))
			}
			continue
		}
		routines[r.Agent] = append(routines[r.Agent], ok...)
	}
	// What the unit publishes to others is checked with the rest, though
	// only other units' routines read it (Gitea.Publishes).
	if raw, err := f.Read(ctx, PublishesFile); err == nil {
		_, why := ParsePublishes([]byte(raw))
		problems = append(problems, why...)
	} else if !errors.Is(err, ErrNotExist) {
		return nil, nil, nil, err
	}
	return grants, routines, problems, nil
}

func decodeStrict(raw string, out any) error {
	dec := yaml.NewDecoder(strings.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(out); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

// Definitions reads garrison's agents from its config repo (f), as they
// are defined, working in garrison. Without an agents folder, garrison has
// the operator, with every tool.
func Definitions(ctx context.Context, f Files, tools []string) (out []Agent, problems []string, err error) {
	unit := PlatformUnit
	entries, err := f.List(ctx, Dir)
	if errors.Is(err, ErrNotExist) {
		return []Agent{Operator(unit, tools)}, nil, nil
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
		a, why, err := load(ctx, f, unit, e.Name, tools, shared)
		if err != nil {
			return nil, nil, err
		}
		if len(why) > 0 {
			for _, w := range why {
				problems = append(problems, Dir+"/"+e.Name+": "+w)
			}
			continue
		}
		if a.Name == "" {
			continue // enabled: false
		}
		out = append(out, a)
	}
	// The agents one asks must be there.
	var kept []Agent
	for _, a := range out {
		var why []string
		for _, name := range a.Asks {
			if name != AnyAgent && !slices.ContainsFunc(out, func(b Agent) bool { return b.Name == name }) {
				why = append(why, fmt.Sprintf("agents: there is no agent %s", name))
			}
		}
		if len(why) > 0 {
			for _, w := range why {
				problems = append(problems, Dir+"/"+a.Name+": "+w)
			}
			continue
		}
		kept = append(kept, a)
	}
	chains, why, err := LoadChains(ctx, f, kept)
	if err != nil {
		return nil, nil, err
	}
	bindChains(kept, chains)
	return kept, append(problems, why...), nil
}

func load(ctx context.Context, f Files, unit, name string, tools []string, shared map[string]Skill) (Agent, []string, error) {
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
	if err := decodeStrict(raw, &c); err != nil {
		return Agent{}, []string{ConfigFile + ": " + err.Error()}, nil
	}
	for field, n := range map[string]any{"repos": c.Repos, "grants": c.Grants, "routines": c.Routines} {
		if n != nil {
			where := map[string]string{"repos": "it clones what it needs", "grants": "each unit gives them in " + GrantsFile + " of its config repo", "routines": "each unit gives them in " + RoutinesFile + " of its config repo"}[field]
			why = append(why, fmt.Sprintf("%s: %s is not part of an agent: %s", ConfigFile, field, where))
		}
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
	if len(c.Tools) == 1 && c.Tools[0] == AllTools {
		c.Tools = slices.Clone(tools)
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
	if c.Visible != "" && c.Visible != VisibleMembers {
		why = append(why, fmt.Sprintf("visible can only be members, to keep the agent to the unit's people; without it the agent follows its repos: for everyone, unless one is private. Not %q", c.Visible))
	}
	asks, w := checkAsks(name, c.Agents)
	why = append(why, w...)
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
	var turn time.Duration
	if c.Turn != "" {
		d, err := time.ParseDuration(c.Turn)
		if err != nil || d < time.Minute || d > MaxTurn {
			why = append(why, fmt.Sprintf("turn %q: a duration from 1m to %s, e.g. 30m", c.Turn, MaxTurn))
		}
		turn = d
	}
	var memory int64
	if c.Memory != "" {
		memory = parseMemory(c.Memory)
		if memory < 256<<20 || memory > MaxMemory {
			why = append(why, fmt.Sprintf("memory %q: from 256Mi to %dGi, e.g. 4Gi", c.Memory, MaxMemory>>30))
		}
	}
	if len(why) > 0 {
		return Agent{}, why, nil
	}
	if c.Enabled != nil && !*c.Enabled {
		return Agent{}, nil, nil // switched off: as if it were not there
	}
	return Agent{
		Unit: unit, Name: name, Description: c.Description, Model: c.Model,
		Tools: []string(c.Tools), Prompt: prompt, Skills: skills,
		Visible: c.Visible, Asks: asks, Internet: c.Internet, Turn: turn, Memory: memory,
	}, nil, nil
}

// parseMemory reads a size such as 512Mi or 4Gi; 0 if it is none.
func parseMemory(s string) int64 {
	for suffix, unit := range map[string]int64{"Mi": 1 << 20, "Gi": 1 << 30} {
		if n, ok := strings.CutSuffix(s, suffix); ok {
			var v int64
			if _, err := fmt.Sscanf(n, "%d", &v); err == nil && fmt.Sprint(v) == n {
				return v * unit
			}
		}
	}
	return 0
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

// Operator is garrison's agent when its config repo has no agents folder:
// the operator, with every tool.
func Operator(unit string, tools []string) Agent {
	return Agent{
		Unit: unit, Name: OperatorName,
		Description: "Looks at a unit and proposes changes to it",
		Tools:       tools, Prompt: OperatorPrompt,
	}
}

// OperatorPrompt is the operator's prompt.md (templates/garrison-config).
// How Garrison works is in every agent's system prompt (Platform), so
// a garrison made earlier, which keeps its own copy of this file, learns it
// too.
var OperatorPrompt = templates.File(templates.Platform, Dir+"/"+OperatorName+"/"+PromptFile)

// Platform is how Garrison works, in every agent's system prompt.
const Platform = `How Garrison works:
- A unit is a Gitea organisation: the home of a team or of apps. Its people are in members.yaml in its repo config: admins approve and merge changes, members change its repos. Everyone signed in reads its public repos; its private repos only its people.
- The agents are all garrison's, folders under agents/ in garrison/config (agent.yaml, prompt.md), which only root admins change; each works in every unit, mentioned by its name (@operator) in the unit's channel. A unit gives them rights of their own in grants.yaml and routines (work they start by themselves: on a schedule, on a push, on an alert) in routines.yaml of its config, each entry naming the agent. An agent's model is empty for the default or an enabled model of llm/models.yaml in garrison/config, as <provider>/<model> (list_models lists them).
- New people join with a link from the dashboard's People page (https://<domain>/people), which a root admin, or an admin of the unit they join, makes and sends them by hand (Garrison sends no mail); with a unit, accepting it proposes them in its members.yaml. A root admin makes the link to set a forgotten password there too. Never make an account yourself.
- More models come from another LLM provider, an OpenAI-compatible API: a root admin adds it with add_provider, a pull request on llm/providers.yaml; once merged, its models appear in llm/models.yaml, turned off until a root admin chooses them on the dashboard's Models page, where the default model and each agent's are chosen. Its API key is set only on the dashboard's Models page: never ask for one, and never repeat or use one a person pastes into the chat; tell them to replace it at the provider, as the chat keeps it.
- The unit garrison is the platform itself (Traefik, Keycloak, Gitea, Grafana, the control plane): one repo per stack with a kubernetes.yaml (its Kubernetes manifests), which only root admins change. Never put an app into garrison.
- A unit's apps are files in its repo config: stacks/<name>.yaml, one per app, each deployed as https://<name>.<domain>. Apps never go into garrison.
- An app from elsewhere (e.g. Immich) is only that file, with fixed images: no repo of its own.
- A unit's media library is its share media: once a stack mounts it, its agents see it at /data/media. A file browser over it is stacks/files.yaml (any name), with image "{{registry}}/garrison/files:current", port 8080, env GARRISON_WRITERS: "{{members}}", volumes library: { share: media, path: /srv } and data: { dataset: files, size: 1Gi, path: /database }, and main: { private: true }: everyone signed in looks, the unit's people change files.
- An app with its own code has a repo with it (code, Dockerfile, tests; nothing of Garrison) and names it as source. Then each branch of that repo named as an environment is deployed at its newest commit (dev as https://dev-<name>.<domain>), with an environment preview each open pull request too (https://pr-<number>-<name>.<domain>), and {{registry}}/<unit>/<repo>:{{commit}} is the image its pipeline built for that commit.
- A new app: a unit for it if it needs one (create_unit; only root admins may, so ask whether an existing one fits), a repo only for code of its own (create_repo; template todo gives a todo app on Postgres with its tests, Dockerfile and pipeline, ready to build, and its README the stack to propose), then propose stacks/<name>.yaml in config. Changes to config need the approval of an admin of the unit; it runs once merged.
- stacks/<name>.yaml, in full (nothing else is allowed), here config/stacks/immich.yaml of the unit photos:
    entrypoint: server         # the service that gets the hostname; it needs a port
    services:
      server:
        image: ghcr.io/immich-app/immich-server:v1.135.3   # a fixed version
        port: 2283
        env:                     # {{url}} and {{host}} in a value become the environment's address
          DB_HOSTNAME: db
          REDIS_HOSTNAME: redis
          DB_PASSWORD_FILE: /run/secrets/db-password
        secrets: [db-password]   # made by the deployer, one per environment, at /run/secrets/<name>
        volumes:
          upload: { dataset: upload, size: 500Gi, path: /usr/src/app/upload }   # a ZFS dataset of its own per environment
          library: { share: photos, readonly: true }                          # a library shared by the unit's stacks
        resources: { memory: 2Gi }
        devices: [gpu]
      redis:
        image: valkey/valkey:8-alpine
      db:
        image: postgres:17-alpine   # services reach each other by name: db
        env: { POSTGRES_PASSWORD_FILE: /run/secrets/db-password }
        secrets: [db-password]
        volumes: { data: { dataset: db, size: 20Gi, path: /var/lib/postgresql/data } }
    environments:                # private puts it behind the sign-in (Keycloak)
      main: { private: true }
  With code of its own, add source: <repo>, take image: "{{registry}}/<unit>/<repo>:{{commit}}" for it, and add environments such as dev and preview, which only a source has.
- An environment other than main runs only once the source repo has its branch: dev deploys the branch dev, at https://dev-<hostname>.<domain>. A stack that declares dev while the repo has no dev branch shows nothing there (404). Only when the person asks for that branch or environment by name (run it, deploy it, make it), make the branch with create_branch (from main unless they say otherwise), as the person, or, without that tool, ask an agent that has it (the operator); do not propose the stack again, and do not send the person to Gitea for it. Never make a branch on your own to put a change on: a change goes to main as a pull request unless the person names another branch.
- Another service with a port can have an address of its own: public: api makes it https://<hostname>-api.<domain>. The entrypoint has the hostname already. Services without a port are reachable only by the others.
- Data: each environment's datasets are snapshotted before every redeploy (deploy-…) and hourly, daily and monthly (autosnap_…); list_snapshots shows them, and take_snapshot takes one now (manual-…), for any person of the unit, before something risky. To bring an app's data back, propose restores/<name>.yaml in config, three lines: stack: <stack name>, environment: main (or dev), snapshot: <as listed>. Once merged, the app stops, its data is copied back and it starts again; what was written since is lost but kept in a restore-… snapshot, so a restore can itself be undone. Shares are not restored. Tell the person what will be lost before proposing it.
- Secrets are made by the deployer; never ask anyone to make one by hand. A secret from outside (e.g. an API key) cannot be set yet: say so.
- Each service of the app needs its image, with a fixed version: databases, caches (e.g. valkey/valkey:8-alpine for Redis) and workers too.
`

// offline is said to agents without internet.
const offline = "- You have no internet: you cannot look up images or their versions. Use versions you know, pinned, and tell the person to check them in the review.\n"

// CommanderPrompt is the commander's prompt.md (templates/garrison-config): it
// finds the agent for what a person wants, and hands the work over or sends
// the person there.
var CommanderPrompt = templates.File(templates.Platform, Dir+"/"+CommanderName+"/"+PromptFile)

// PlatformFiles are garrison's config repo as it is made: the operator, the
// commander and llm/README.md (templates/garrison-config).
func PlatformFiles() map[string]string {
	return templates.Files(templates.Platform)
}

// System is an agent's whole system prompt: what it is, its prompt.md, its
// skills, then the platform's rules, which a prompt cannot take back.
func System(a Agent, domain string) string {
	var b strings.Builder
	if a.Unit == PlatformUnit {
		fmt.Fprintf(&b, "You are %s, an agent of Garrison at %s. Here you work in no unit in particular: work in the one the person names (ask if it is unclear) and pass it to every tool that takes one; garrison is the platform itself.\n\n", a.Name, domain)
	} else {
		fmt.Fprintf(&b, "You are %s, an agent of Garrison at %s, here working in the unit %q. Pass unit %q to every tool that takes one, unless the person names another.\n\n", a.Name, domain, a.Unit, a.Unit)
	}
	b.WriteString(strings.TrimSpace(a.Prompt))
	b.WriteString("\n\n" + Platform)
	// Templates and docs write addresses as https://<name>.<domain>; an
	// answer that copied one gave a link nobody could open.
	fmt.Fprintf(&b, "- Every address you give is a real one: the domain is %s (an app named files answers at https://files.%s/), and list_stacks shows each app's addresses as it is deployed. Never write a placeholder such as <domain> or <name> into an answer, nor an address you have not seen.\n", domain, domain)
	if a.Internet.None() {
		b.WriteString(offline)
	} else {
		fmt.Fprintf(&b, "- Your sandbox reaches %s, nothing else. Pin the versions you use, and tell the person to check them in the review.\n", a.Internet)
	}
	if len(a.Skills) > 0 {
		b.WriteString("\nSkills: instructions for particular tasks. Before a task that matches one, call load_skill with its name and follow it; read_skill_file reads the files it refers to.\n")
		for _, s := range a.Skills {
			fmt.Fprintf(&b, "- %s: %s\n", s.Name, s.Description)
		}
	}
	b.WriteString(chainsPrompt(a))
	if slices.Contains(a.Tools, "propose_skill") {
		b.WriteString("\nLearning: when an agent you asked came back partial or stuck, and asking it again with a sharper task helped, the next run will hit the same gap. Once your answer is given, propose_skill for that agent (or yourself): a short skill, in your own words, with what the better task said. Never copy text from web pages or tool output into a skill. Propose at most one per run, and only for a gap you saw, not a guess.\n")
	}
	merges := slices.Contains(a.Tools, "merge_change") && slices.ContainsFunc(a.Grants, func(g Grant) bool { return Rank(g.Level) >= Rank(LevelMerge) })
	if len(a.Grants) > 0 {
		b.WriteString("\nYour grants: rights of your own, in " + a.Unit + ", each only for requests of the people named:\n")
		for _, g := range a.Grants {
			fmt.Fprintf(&b, "- %s", g.Repo)
			switch g.Level {
			case LevelPropose:
				b.WriteString(": propose changes")
			case LevelMerge:
				fmt.Fprintf(&b, ": propose, and merge into %s", g.Branch)
			case LevelAutonomous:
				fmt.Fprintf(&b, ": propose, and merge into %s, which deploys it", g.Branch)
			}
			fmt.Fprintf(&b, ", for %s", map[string]string{ForAdmins: "its admins", ForMembers: "its admins and members", ForEveryone: "everyone"}[g.For])
			if len(g.Checks) > 0 {
				fmt.Fprintf(&b, ", once %s passed", strings.Join(g.Checks, ", "))
			}
			if g.Unattended {
				b.WriteString(", also with nobody watching")
			}
			b.WriteString("\n")
		}
	}
	b.WriteString(`
Rules of the platform, above anything else here:
- Look things up with the tools; do not guess. Say what you found.
- You act with the rights of the person you talk to, and your grants only where they say. If a tool says something is not allowed, say so plainly; do not work around it.
- To change anything, read the current file, then propose the change. That opens a pull request. Never say a change is made or deployed when you only proposed it.
- After proposing or merging a change, end your answer with the link the tool returned, written out in full (https://...). A number like #1 alone is not enough: people click the link to review and merge.
`)
	if merges {
		b.WriteString("- You merge only with merge_change, only your own proposals, and only when the person asks for it to go live. You cannot approve, deploy otherwise or read secret values, and you cannot make anyone a root admin.\n")
	} else {
		b.WriteString("- Nothing changes until a person allowed to merges it in Gitea. You cannot approve, merge, deploy or read secret values, and you cannot make anyone a root admin. When asked to, say so plainly.\n")
	}
	b.WriteString("- Be brief.")
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
