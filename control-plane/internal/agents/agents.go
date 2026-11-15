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
	// CommanderName is the agent of system that finds the agent for a task.
	CommanderName = "commander"
	// SystemNamespace is the platform's own namespace.
	SystemNamespace = "system"
	// AnyAgent, as agents: any, lets an agent ask every agent the person may
	// use. Only system's agents, which root admins approve, may.
	AnyAgent  = "any"
	maxPrompt = 32000
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
	Visible     string   // members: only the namespace's people; empty: as its repos (WorksOn)
	Asks        []string // agents it may ask for work, <namespace>/<agent>
	Grants      []Grant  // rights of its own
	Routines    []Routine
	Internet    Internet // what its sandbox reaches through the proxy
}

// ID is the agent's model id in the chat, and the name people mention:
// <namespace>/<agent>.
func (a Agent) ID() string { return a.Namespace + "/" + a.Name }

// AsksAny tells whether a may ask every agent the person may use.
func (a Agent) AsksAny() bool { return slices.Contains(a.Asks, AnyAgent) }

// MayAsk tells whether a may hand work to the agent id: one of its agents,
// or any but itself with agents: any. Whether the person may use it is the
// agent service's to check.
func (a Agent) MayAsk(id string) bool {
	return id != a.ID() && (a.AsksAny() || slices.Contains(a.Asks, id))
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
	Description string    `yaml:"description"`
	Model       string    `yaml:"model"`
	Tools       []string  `yaml:"tools"`
	Skills      []string  `yaml:"skills"`
	Repos       []string  `yaml:"repos"`
	Visible     string    `yaml:"visible,omitempty"` // only "members"
	Agents      askList   `yaml:"agents,omitempty"`
	Grants      []Grant   `yaml:"grants,omitempty"`
	Routines    []Routine `yaml:"routines,omitempty"`
	Internet    Internet  `yaml:"internet,omitempty"`
	Enabled     *bool     `yaml:"enabled,omitempty"`
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
		if a.Name == "" {
			continue // enabled: false
		}
		out = append(out, a)
	}
	// Agents of this namespace that one asks must be here; those of other
	// namespaces are looked up when asked.
	var kept []Agent
	for _, a := range out {
		var why []string
		for _, id := range a.Asks {
			if id == AnyAgent {
				continue
			}
			ns, name, _ := strings.Cut(id, "/")
			if ns == namespace && !slices.ContainsFunc(out, func(b Agent) bool { return b.Name == name }) {
				why = append(why, fmt.Sprintf("agents: there is no agent %s", id))
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
	return kept, problems, nil
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
	if c.Visible != "" && c.Visible != VisibleMembers {
		why = append(why, fmt.Sprintf("visible can only be members, to keep the agent to the namespace's people; without it the agent follows its repos: for everyone, unless one is private. Not %q", c.Visible))
	}
	why = append(why, checkAsks(namespace+"/"+name, c.Agents)...)
	grants, w := checkGrants(c.Grants)
	why = append(why, w...)
	routines, w := checkRoutines(c.Routines)
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
	if len(why) > 0 {
		return Agent{}, why, nil
	}
	if c.Enabled != nil && !*c.Enabled {
		return Agent{}, nil, nil // switched off: as if it were not there
	}
	return Agent{
		Namespace: namespace, Name: name, Description: c.Description, Model: c.Model,
		Tools: c.Tools, Repos: c.Repos, Prompt: prompt, Skills: skills,
		Visible: c.Visible, Asks: []string(c.Agents), Grants: grants, Routines: routines,
		Internet: c.Internet,
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

// OperatorPrompt is the operator's prompt.md. How the home cloud works is
// in every agent's system prompt (Platform), so a namespace made earlier,
// which keeps its own copy of this file, learns it too.
const OperatorPrompt = `You answer questions about the namespace's apps and stacks, and set up and change them, using your tools: you look things up, make namespaces and repos where the person may, and propose changes.

- Logs: service_logs. Metrics: service_usage for CPU and memory of a stack, query_metrics for any PromQL.
- To add, remove or change someone in the namespace, propose a new members.yaml; it needs the approval of an admin who neither wrote the change nor has their role changed by it, so nobody can raise their own rights. Root admins are managed outside the namespaces and cannot be changed from here.
- When someone asks for a new app, ask what you need to know first (which namespace; anything you would otherwise guess), then make what is missing and propose the rest.
`

// Platform is how the home cloud works, in every agent's system prompt.
const Platform = `How the home cloud works:
- A namespace is a Gitea organisation: the home of a team or of apps. Its people are in members.yaml in its repo config: admins approve and merge changes, members change its repos. Everyone signed in reads its public repos; its private repos only its people. Its agents are folders under agents/ in config (agent.yaml, prompt.md).
- The namespace system is the platform itself (Traefik, Keycloak, Gitea, Grafana, the control plane): one repo per stack with a compose.yaml, which only root admins change. Never put an app into system.
- A namespace's apps are files in its repo config: stacks/<name>.yaml, one per app, each deployed as https://<name>.<domain>. Apps never go into system.
- An app from elsewhere (e.g. Immich) is only that file, with fixed images: no repo of its own.
- An app with its own code has a repo with it (code, Dockerfile, tests; nothing of the home cloud) and names it as source. Then each branch of that repo named as an environment is deployed at its newest commit (dev as https://dev-<name>.<domain>), with an environment preview each open pull request too (https://pr-<number>-<name>.<domain>), and {{registry}}/<namespace>/<repo>:{{commit}} is the image its pipeline built for that commit.
- A new app: a namespace for it if it needs one (create_namespace; only root admins may, so ask whether an existing one fits), a repo only for code of its own (create_repo), then propose stacks/<name>.yaml in config. Changes to config need the approval of an admin of the namespace; it runs once merged.
- stacks/<name>.yaml, in full (nothing else is allowed), here config/stacks/immich.yaml of the namespace photos:
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
          library: { share: photos, readonly: true }                          # a library shared by the namespace's stacks
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
  With code of its own, add source: <repo>, take image: "{{registry}}/<namespace>/<repo>:{{commit}}" for it, and add environments such as dev and preview, which only a source has.
- Another service with a port can have an address of its own: public: api makes it https://<hostname>-api.<domain>. The entrypoint has the hostname already. Services without a port are reachable only by the others.
- Data: each environment's datasets are snapshotted before every redeploy (deploy-…) and hourly, daily and monthly (autosnap_…); list_snapshots shows them. To bring an app's data back, propose restores/<name>.yaml in config, three lines: stack: <stack name>, environment: main (or dev), snapshot: <as listed>. Once merged, the app stops, its data is copied back and it starts again; what was written since is lost but kept in a restore-… snapshot, so a restore can itself be undone. Shares are not restored. Tell the person what will be lost before proposing it.
- Secrets are made by the deployer; never ask anyone to make one by hand. A secret from outside (e.g. an API key) cannot be set yet: say so.
- Each service of the app needs its image, with a fixed version: databases, caches (e.g. valkey/valkey:8-alpine for Redis) and workers too.
`

// offline is said to agents without internet.
const offline = "- You have no internet: you cannot look up images or their versions. Use versions you know, pinned, and tell the person to check them in the review.\n"

// OperatorFiles are the files of the operator's folder, as written into a
// new config repo.
func OperatorFiles(tools []string) map[string]string {
	cfg, _ := yaml.Marshal(config{Description: "Looks at the namespace and proposes changes to it", Tools: tools})
	return map[string]string{
		Dir + "/README.md": `# Agents

One folder per agent. People mention an agent in the chat as ` + "`@<namespace>/<folder>`" + `.

- ` + "`agent.yaml`" + `: ` + "`description`" + `, ` + "`model`" + ` (empty for the default), ` + "`tools`" + ` (the gateway tools it may use), ` + "`skills`" + ` (shared skills from ` + "`skills/`" + ` at the top of this repo), ` + "`repos`" + ` and ` + "`internet`" + ` (what its sandbox reaches: ` + "`packages`" + `, the package registries; ` + "`all`" + `; or a list of ` + "`packages`" + ` and domains; none without it).
- ` + "`routines`" + `: work it does by itself, for an ` + "`owner`" + `, with a ` + "`task`" + `: on a ` + "`schedule`" + ` (cron, e.g. ` + "`\"30 6 * * mon-fri\"`" + `, in the home cloud's time zone), ` + "`every`" + ` (e.g. ` + "`6h`" + `), ` + "`on_push`" + ` (` + "`{repo, branch}`" + `) or ` + "`on_alert`" + ` (label values of this namespace's alerts, e.g. ` + "`{alertname: ServiceFailing}`" + `). Its answer goes to the namespace's channel.
- ` + "`prompt.md`" + `: its instructions. The platform adds its own rules after them.
- ` + "`skills/<name>/SKILL.md`" + `: skills of this agent only, in the Agent Skills format.

An agent acts with the rights of the person talking to it. Changes here need an admin's approval, like members.yaml.
`,
		Dir + "/" + OperatorName + "/" + ConfigFile: "# The operator: answers questions about the namespace and proposes changes.\n" + string(cfg),
		Dir + "/" + OperatorName + "/" + PromptFile: OperatorPrompt,
	}
}

// CommanderPrompt is the commander's prompt.md: it finds the agent for what
// a person wants, and hands the work over or sends the person there.
const CommanderPrompt = `You are the commander. People come to you when they do not know which agent to ask. You do not do the work yourself: you find the agent for it, then hand the work over or tell the person whom to talk to.

1. Call find_agents, without in_namespace. It lists the agents this person may use here: what each does, its tools, skills, repos and rights, and which namespace this channel belongs to.
2. Pick the agent whose description, tools and repos fit. Prefer the namespace the person names, then this channel's namespace. Each namespace has an operator, which sets up namespaces, repos and apps and reads logs and metrics.
3. Then one of these:
   - A clear task one agent can finish in one go (a screenshot, a new repo, why an app does not start): hand it over with ask_agent. Put everything it needs into task (namespace, repo, branch, what exactly), what it must not do into limits, and what a good answer is into done_when. In a channel, set background: the agent posts its answer here itself. Tell the person whom you asked and for what.
   - Work with back and forth (building a feature, setting up a new app, anything the person decides along the way): do not hand it over. Tell the person which agent to talk to, as @<namespace>/<agent>, and in one sentence why it fits.
   - If you cannot tell what the person wants, or which of two agents fits, ask one short question first.
4. If no agent fits, say so, and what an agent for it would need (tools, repos): the namespace's operator can propose one.

Pass on what an agent answered as it is; never say it did something it did not say it did.
`

// CommanderFiles are the files of the commander's folder in system's config
// repo: it may ask any agent the person may use, and needs no tools.
func CommanderFiles() map[string]string {
	cfg, _ := yaml.Marshal(config{Description: "Finds the agent for what you want, and hands the work to it or tells you whom to ask", Tools: []string{}, Agents: askList{AnyAgent}})
	return map[string]string{
		Dir + "/" + CommanderName + "/" + ConfigFile: "# The commander: finds the agent for a task. It acts only through other agents, as the person.\n" + string(cfg),
		Dir + "/" + CommanderName + "/" + PromptFile: CommanderPrompt,
	}
}

// System is an agent's whole system prompt: what it is, its prompt.md, its
// skills, then the platform's rules, which a prompt cannot take back.
func System(a Agent, domain string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "You are %s, an agent of the namespace %q of a home cloud at %s. Pass namespace %q to every tool that takes one.\n\n", a.ID(), a.Namespace, domain, a.Namespace)
	b.WriteString(strings.TrimSpace(a.Prompt))
	b.WriteString("\n\n" + Platform)
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
	merges := slices.Contains(a.Tools, "merge_change") && slices.ContainsFunc(a.Grants, func(g Grant) bool { return Rank(g.Level) >= Rank(LevelMerge) })
	if len(a.Grants) > 0 {
		b.WriteString("\nYour grants: rights of your own, in " + a.Namespace + ", each only for requests of the people named:\n")
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
