package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"homecloud/control-plane/internal/agents"
	"homecloud/control-plane/internal/apply"
	"homecloud/control-plane/internal/gitops"
)

// Person is who is asking: the signed-in user behind a tool call.
type Person struct {
	Name  string `json:"name"`
	Email string `json:"email"`
	Admin bool   `json:"admin"` // member of root-admins
	// Token is their Keycloak access token, for tools that act as them
	// elsewhere, e.g. the browser. Never shown or logged. Empty in a run
	// without a person.
	Token string `json:"-"`
	// Set by the agent service (see ServiceKey):
	StartedBy  string `json:"-"` // what started the run: person, agent <id>, routine <agent>/<name>
	Unattended bool   `json:"-"` // a run without a person: Name is the agent's account
	Owner      string `json:"-"` // in such a run, the routine's owner, for whom grants are checked
}

// SystemNamespace is the namespace the deployer deploys.
const SystemNamespace = "system"

// Tools are what the operator agents may do. Reads of the cluster (stacks,
// logs, metrics) are open to everyone signed in, as everything is; reads and
// proposals in Gitea go as the person, so Gitea applies their own rights.
// Nothing here merges or deploys, except merge_change for agents whose
// grants allow it (grants.go).
type Tools struct {
	Gitea      *Gitea
	Run        apply.Runner // docker CLI
	WorkDir    string       // the deployer's state, read-only
	Prometheus string       // e.g. http://prometheus_prometheus:9090
	Browser    *Browser     // for screenshot; nil: none
	HTTP       *http.Client
}

const maxToolOutput = 12000

// Spec is a tool as MCP describes it.
type Spec struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

func spec(name, desc string, props map[string]any, required ...string) Spec {
	if props == nil {
		props = map[string]any{}
	}
	if required == nil {
		required = []string{}
	}
	return Spec{Name: name, Description: desc, InputSchema: map[string]any{
		"type": "object", "properties": props, "required": required, "additionalProperties": false,
	}}
}

func str(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }

var nsArg = str("the namespace, i.e. the Gitea organisation, e.g. system")

// Specs are the tools of the plain /mcp endpoint.
func (t *Tools) Specs() []Spec { return SpecsFor(nil) }

// SpecsFor are an agent's tools: the base tools its agent.yaml lists, and
// the skill tools if it has skills. A nil agent has every base tool.
func SpecsFor(a *agents.Agent) []Spec {
	if a == nil {
		// Merging is only ever an agent's, under its grants.
		return slices.DeleteFunc(baseSpecs(), func(s Spec) bool { return s.Name == "merge_change" })
	}
	var out []Spec
	for _, s := range baseSpecs() {
		if slices.Contains(a.Tools, s.Name) {
			out = append(out, s)
		}
	}
	if len(a.Skills) > 0 {
		out = append(out,
			spec("load_skill", "Loads one of your skills: its instructions and the list of its files. Call it before a task the skill is for.", map[string]any{"name": str("the skill's name")}, "name"),
			spec("read_skill_file", "Reads a file of one of your skills, as listed by load_skill.", map[string]any{"skill": str("the skill's name"), "path": str("the file's path inside the skill, e.g. template.svg")}, "skill", "path"),
		)
	}
	return out
}

func baseSpecs() []Spec {
	return []Spec{
		spec("list_namespaces", "Lists the namespaces (Gitea organisations).", nil),
		spec("list_stacks", "Lists a namespace's stacks with the commit the deployer deployed, and each of their Swarm services with running/desired replicas and image.", map[string]any{"namespace": nsArg}, "namespace"),
		spec("stack_status", "Shows the tasks of one stack: their state and any error, newest first. Use it to see why a service does not run.", map[string]any{"namespace": nsArg, "stack": str("stack name, e.g. grafana")}, "namespace", "stack"),
		spec("service_logs", "Shows the last lines of a service's logs.", map[string]any{
			"namespace": nsArg,
			"service":   str("Swarm service name, <stack>_<service>, e.g. grafana_grafana"),
			"lines":     map[string]any{"type": "integer", "description": "how many lines, at most 500; default 100"},
		}, "namespace", "service"),
		spec("service_usage", "Shows CPU and memory use of a stack's services over the last minutes, from Prometheus.", map[string]any{"namespace": nsArg, "stack": str("stack name, e.g. grafana")}, "namespace", "stack"),
		spec("query_metrics", "Runs an instant PromQL query against Prometheus. It has node-exporter metrics (node_*) and cAdvisor container metrics (container_*, with the label container_label_com_docker_swarm_service_name).", map[string]any{"query": str("PromQL expression")}, "query"),
		spec("list_members", "Shows who may change a namespace: its admins and members, from members.yaml in its repo config. Not for system, which root admins change.", map[string]any{"namespace": nsArg}, "namespace"),
		spec("list_repos", "Lists the repos of a namespace. In system, each repo with a compose.yaml is a stack.", map[string]any{"namespace": nsArg}, "namespace"),
		spec("list_files", "Lists a directory of a repo's main branch.", map[string]any{"namespace": nsArg, "repo": str("repo name"), "path": str("directory, empty for the top")}, "namespace", "repo"),
		spec("read_file", "Reads a file of a repo's main branch.", map[string]any{"namespace": nsArg, "repo": str("repo name"), "path": str("file path, e.g. compose.yaml")}, "namespace", "repo", "path"),
		spec("list_changes", "Lists a namespace's proposed changes (open pull requests) with their diffs and links.", map[string]any{"namespace": nsArg}, "namespace"),
		spec("propose_change", "Proposes a new version of one file as a pull request, as the person asking. Nothing changes until a person allowed to merges it in Gitea; give them the link.", map[string]any{
			"namespace":   nsArg,
			"repo":        str("repo name"),
			"path":        str("file path"),
			"content":     str("the complete new content of the file"),
			"title":       str("short title of the change"),
			"description": str("why, and what it does"),
		}, "namespace", "repo", "path", "content", "title"),
		spec("propose_patch", "Proposes changes to several files at once, as a patch (the output of git diff, with --binary for binary files), as a pull request by the person asking. Without branch, a new branch from base and a new pull request; with the branch of an earlier change, the patch goes on top of it and its pull request shows it. Nothing changes until a person allowed to merges it in Gitea; give them the link.", map[string]any{
			"namespace":   nsArg,
			"repo":        str("repo name"),
			"patch":       str("the changes, as git diff prints them"),
			"title":       str("short title of the change"),
			"description": str("why, and what it does"),
			"base":        str("branch the change is for; default main"),
			"branch":      str("the branch of an earlier change (change/...) to add to; empty for a new change"),
		}, "namespace", "repo", "patch", "title"),
		spec("merge_change", "Merges a pull request of one of your proposals, with your own account, if one of your grants allows merging into its base branch for the person you talk to and its checks have passed. A merge into a branch that deploys (main, dev) deploys it. If it is refused, say why; do not work around it.", map[string]any{
			"namespace": nsArg,
			"repo":      str("repo name"),
			"number":    map[string]any{"type": "integer", "description": "the pull request's number"},
		}, "namespace", "repo", "number"),
		spec("browse", "Uses a page of the home cloud as the person you talk to, signed in as them, one step per call: open a page (url), click an element (click), or type into one (type_into and text, with submit to press Enter). Each call answers with the page as it is then: its text and its links, buttons and fields, numbered. The tab stays open between calls for a few minutes. Only pages at https://<name>.<domain> load. It acts as the person: click nothing that deletes, sends or pays unless they asked for exactly that.", map[string]any{
			"url":       str("the page to open, e.g. https://pr-7-todo.example.com/; empty to act on the open page"),
			"click":     map[string]any{"type": "integer", "description": "the number of the element to click"},
			"type_into": map[string]any{"type": "integer", "description": "the number of the field to type into"},
			"text":      str("what to type"),
			"submit":    map[string]any{"type": "boolean", "description": "press Enter after typing"},
		}),
		spec("screenshot", "Takes a screenshot of a page of the home cloud, signed in as the person asking, and proposes it as a PNG file in a repo: a pull request, as with propose_change. Only pages at https://<name>.<domain> load; requests elsewhere are refused.", map[string]any{
			"namespace":   nsArg,
			"repo":        str("repo the PNG goes into"),
			"path":        str("file path of the PNG, ending in .png, e.g. docs/screenshots/todo.png"),
			"url":         str("the page, e.g. https://todo.example.com/"),
			"width":       map[string]any{"type": "integer", "description": "window width in pixels, 320 to 2560; default 1280"},
			"height":      map[string]any{"type": "integer", "description": "window height in pixels, 240 to 2000; default 800"},
			"full_page":   map[string]any{"type": "boolean", "description": "the whole page instead of the window; default false"},
			"title":       str("short title of the change"),
			"description": str("why, and what it shows"),
		}, "namespace", "repo", "path", "url", "title"),
	}
}

var swarmName = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,100}$`)

type args struct {
	Namespace, Stack, Service, Repo, Path, Content, Title, Description, Query, Name, Skill string
	Patch, Base, Branch                                                                    string
	Lines, Width, Height, Number, Click                                                    int
	TypeInto                                                                               int `json:"type_into"`
	Text                                                                                   string
	Submit                                                                                 bool
	URL                                                                                    string `json:"url"`
	FullPage                                                                               bool   `json:"full_page"`
}

// Call runs one tool for person, talking to agent (nil: the plain /mcp
// endpoint, with every base tool). A tool the agent lacks is refused.
func (t *Tools) Call(ctx context.Context, p Person, agent *agents.Agent, name string, raw json.RawMessage) (string, error) {
	if !slices.ContainsFunc(SpecsFor(agent), func(s Spec) bool { return s.Name == name }) {
		return "", fmt.Errorf("%w: this agent has no tool %q", ErrInvalid, name)
	}
	var a args
	if len(raw) > 0 && string(raw) != "null" {
		if err := json.Unmarshal(raw, &a); err != nil {
			return "", fmt.Errorf("%w: arguments: %v", ErrInvalid, err)
		}
	}
	// A run without a person acts with the agent's account, which may write
	// to its granted repos: only propose_patch and merge_change, which check
	// the grants, change anything then. The browser needs a person's sign-in.
	if p.Unattended && (name == "propose_change" || name == "screenshot" || name == "browse") {
		return "", refused{name + " needs a person; this run has none. Use propose_patch"}
	}
	var out string
	var err error
	if name == "load_skill" || name == "read_skill_file" {
		out, err = t.skill(ctx, p, agent, name, a)
	} else {
		out, err = t.call(ctx, p, agent, name, a)
	}
	if len(out) > maxToolOutput {
		out = out[:maxToolOutput] + "\n[truncated]"
	}
	return out, err
}

func (t *Tools) call(ctx context.Context, p Person, agent *agents.Agent, name string, a args) (string, error) {
	switch name {
	case "list_namespaces":
		nss, err := t.Gitea.Namespaces(ctx, p.Name)
		if err != nil {
			return "", err
		}
		var b strings.Builder
		for _, n := range nss {
			fmt.Fprintf(&b, "%s\t%s\n", n.Name, n.Description)
		}
		return b.String(), nil
	case "list_stacks":
		stacks, err := t.Stacks(ctx, a.Namespace)
		if err != nil {
			return "", err
		}
		if len(stacks) == 0 {
			return "No stacks are deployed in this namespace. Each repo with a stack.yaml is deployed from its branches, once its image is built.", nil
		}
		var b strings.Builder
		for _, s := range stacks {
			fmt.Fprintf(&b, "%s (commit %.12s)\n", s.Name, s.Commit)
			for _, svc := range s.Services {
				fmt.Fprintf(&b, "  %s %s %s\n", svc.Name, svc.Replicas, svc.Image)
			}
		}
		return b.String(), nil
	case "stack_status":
		if err := t.ownStack(a.Namespace, a.Stack); err != nil {
			return "", err
		}
		out, err := t.Run(ctx, nil, nil, "docker", "stack", "ps", "--no-trunc", "--format", "{{.Name}}\t{{.CurrentState}}\t{{.Error}}", a.Stack)
		return firstLines(string(out), 40), err
	case "service_logs":
		stack, _, ok := strings.Cut(a.Service, "_")
		if !ok || !swarmName.MatchString(a.Service) {
			return "", fmt.Errorf("%w: service %q is not <stack>_<service>", ErrInvalid, a.Service)
		}
		if err := t.ownStack(a.Namespace, stack); err != nil {
			return "", err
		}
		n := a.Lines
		if n <= 0 || n > 500 {
			n = 100
		}
		// Containers log to both streams. The names are arguments, never part of the script.
		out, err := t.Run(ctx, nil, nil, "sh", "-c", `docker service logs --tail "$1" --no-task-ids --timestamps "$2" 2>&1`, "sh", fmt.Sprint(n), a.Service)
		return string(out), err
	case "service_usage":
		if err := t.ownStack(a.Namespace, a.Stack); err != nil {
			return "", err
		}
		sel := fmt.Sprintf(`container_label_com_docker_stack_namespace=%q`, a.Stack)
		cpu, err := t.promQuery(ctx, fmt.Sprintf(`sum by (container_label_com_docker_swarm_service_name) (rate(container_cpu_usage_seconds_total{%s}[5m])) * 100`, sel))
		if err != nil {
			return "", err
		}
		mem, err := t.promQuery(ctx, fmt.Sprintf(`sum by (container_label_com_docker_swarm_service_name) (container_memory_working_set_bytes{%s})`, sel))
		if err != nil {
			return "", err
		}
		return "CPU, percent of one core, 5-minute average:\n" + cpu + "\nMemory in use, bytes:\n" + mem, nil
	case "query_metrics":
		if strings.TrimSpace(a.Query) == "" || len(a.Query) > 2000 {
			return "", fmt.Errorf("%w: query", ErrInvalid)
		}
		return t.promQuery(ctx, a.Query)
	case "list_members":
		if a.Namespace == SystemNamespace {
			return "The namespace system is changed by root admins only; it has no members.yaml.", nil
		}
		roster, err := t.Gitea.Roster(ctx, p.Name, a.Namespace)
		if err != nil {
			return "", err
		}
		return "members.yaml of " + a.Namespace + "/config on main:\n" + roster +
			"\nTo change it, propose a new members.yaml in the repo config. It needs the approval of an admin who did not write the change and whose role it does not change.", nil
	case "list_repos":
		repos, err := t.Gitea.Repos(ctx, p.Name, a.Namespace)
		if err != nil {
			return "", err
		}
		var b strings.Builder
		for _, r := range repos {
			fmt.Fprintf(&b, "%s\t%s\n", r.Name, r.Description)
		}
		return b.String(), nil
	case "list_files":
		entries, err := t.Gitea.List(ctx, p.Name, a.Namespace, a.Repo, a.Path)
		if err != nil {
			return "", err
		}
		var b strings.Builder
		for _, e := range entries {
			fmt.Fprintf(&b, "%s\t%s\n", e.Type, e.Path)
		}
		return b.String(), nil
	case "read_file":
		return t.Gitea.Read(ctx, p.Name, a.Namespace, a.Repo, a.Path)
	case "list_changes":
		changes, err := t.Gitea.Changes(ctx, p.Name, a.Namespace)
		if err != nil {
			return "", err
		}
		if len(changes) == 0 {
			return "No changes are waiting.", nil
		}
		var b strings.Builder
		for _, c := range changes {
			fmt.Fprintf(&b, "%s#%d %q by %s: %s\n%s\n\n", c.Repo, c.Number, c.Title, c.Author, c.URL, firstLines(c.Diff, 60))
		}
		return b.String(), nil
	case "screenshot":
		return t.screenshot(ctx, p, a)
	case "browse":
		if t.Browser == nil {
			return "", errors.New("this installation has no browser")
		}
		page, err := t.Browser.Browse(ctx, p.Name+"\x00"+agentID(agent), p.Token, Action{URL: a.URL, Click: a.Click, Type: a.TypeInto, Text: a.Text, Submit: a.Submit})
		if err != nil {
			if page.URL != "" {
				return "", fmt.Errorf("%w\n%s", err, page)
			}
			return "", err
		}
		return page.String(), nil
	case "propose_patch":
		return t.proposePatch(ctx, p, agent, a)
	case "merge_change":
		return t.mergeChange(ctx, p, agent, a.Namespace, a.Repo, a.Number)
	case "propose_change":
		c, err := t.Gitea.Propose(ctx, p.Name, a.Namespace, a.Repo, a.Path, a.Content, a.Title, a.Description)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("Opened change %s/%s#%d. Nothing is changed yet: a person allowed to merge must review and merge it in Gitea.\nEnd your answer with this link, exactly as it is, so the person can open it:\n%s", c.Namespace, c.Repo, c.Number, c.URL), nil
	}
	return "", fmt.Errorf("%w: unknown tool %q", ErrInvalid, name)
}

// screenshot shoots a page as person and proposes the PNG.
func (t *Tools) screenshot(ctx context.Context, p Person, a args) (string, error) {
	if t.Browser == nil {
		return "", errors.New("no browser is set up")
	}
	if !strings.HasSuffix(strings.ToLower(a.Path), ".png") {
		return "", fmt.Errorf("%w: path %q must end in .png", ErrInvalid, a.Path)
	}
	w, h := a.Width, a.Height
	if w == 0 {
		w = 1280
	}
	if h == 0 {
		h = 800
	}
	if w < 320 || w > 2560 || h < 240 || h > 2000 {
		return "", fmt.Errorf("%w: the window must be 320 to 2560 wide and 240 to 2000 high", ErrInvalid)
	}
	// The repo is checked before the browser runs: a refusal costs nothing.
	if err := t.Gitea.CanWrite(ctx, p.Name, a.Namespace, a.Repo); err != nil {
		return "", err
	}
	shot, err := t.Browser.Shoot(ctx, p.Token, a.URL, w, h, a.FullPage)
	if err != nil {
		return "", err
	}
	desc := strings.TrimSpace(a.Description)
	if desc != "" {
		desc += "\n\n"
	}
	desc += fmt.Sprintf("Screenshot of %s as %s, %dx%d: page title %q.", shot.URL, p.Name, w, h, shot.Title)
	c, err := t.Gitea.Propose(ctx, p.Name, a.Namespace, a.Repo, a.Path, string(shot.PNG), a.Title, desc)
	if err != nil {
		return "", err
	}
	note := ""
	if shot.Blocked > 0 {
		note = fmt.Sprintf(" %d requests to other sites were refused, so parts loaded from elsewhere may be missing.", shot.Blocked)
	}
	return fmt.Sprintf("Took the screenshot of %s (page title %q, %d KB) and opened change %s/%s#%d with %s.%s Nothing is changed yet: a person allowed to merge must review and merge it in Gitea.\nEnd your answer with this link, exactly as it is, so the person can open it:\n%s",
		shot.URL, shot.Title, len(shot.PNG)>>10, c.Namespace, c.Repo, c.Number, a.Path, note, c.URL), nil
}

// skill reads one of the agent's skills from its config repo, as person.
func (t *Tools) skill(ctx context.Context, p Person, agent *agents.Agent, name string, a args) (string, error) {
	want := a.Name
	if name == "read_skill_file" {
		want = a.Skill
	}
	i := slices.IndexFunc(agent.Skills, func(s agents.Skill) bool { return s.Name == want })
	if i < 0 {
		var names []string
		for _, s := range agent.Skills {
			names = append(names, s.Name)
		}
		return "", fmt.Errorf("%w: no skill %q; yours are: %s", ErrInvalid, want, strings.Join(names, ", "))
	}
	sk := agent.Skills[i]
	if name == "read_skill_file" {
		if err := validPath(a.Path); err != nil || a.Path == "" {
			return "", fmt.Errorf("%w: path %q", ErrInvalid, a.Path)
		}
		return t.Gitea.Read(ctx, p.Name, agent.Namespace, ConfigRepo, sk.Dir+"/"+a.Path)
	}
	body, err := t.Gitea.Read(ctx, p.Name, agent.Namespace, ConfigRepo, sk.Dir+"/"+agents.SkillFile)
	if err != nil {
		return "", err
	}
	var files []string
	var walk func(dir, prefix string) error
	walk = func(dir, prefix string) error {
		entries, err := t.Gitea.List(ctx, p.Name, agent.Namespace, ConfigRepo, dir)
		if err != nil {
			return err
		}
		for _, e := range entries {
			switch {
			case e.Type == "dir" && len(files) < 200:
				if err := walk(dir+"/"+e.Name, prefix+e.Name+"/"); err != nil {
					return err
				}
			case e.Type == "file" && e.Name != agents.SkillFile:
				files = append(files, prefix+e.Name)
			}
		}
		return nil
	}
	if err := walk(sk.Dir, ""); err != nil {
		return "", err
	}
	out := body
	if len(files) > 0 {
		out += "\n\nFiles of this skill (read them with read_skill_file):\n- " + strings.Join(files, "\n- ")
	}
	return out, nil
}

// stackNames lists a namespace's deployed Swarm stacks with their commits:
// in system the stacks the deployer deployed, elsewhere one per deployed
// environment of each app, named after its hostname (todo, dev-todo).
func stackNames(state gitops.State, namespace string) map[string]string {
	out := map[string]string{}
	if namespace == SystemNamespace {
		for name, commit := range state.Stacks {
			out[name] = commit
		}
		return out
	}
	for _, a := range state.AppsOf(namespace) {
		out[a.Stack] = a.Commit
	}
	return out
}

// ownStack checks that stack is a deployed stack of namespace.
func (t *Tools) ownStack(namespace, stack string) error {
	if err := validOrg(namespace); err != nil {
		return err
	}
	if !swarmName.MatchString(stack) {
		return fmt.Errorf("%w: stack %q", ErrInvalid, stack)
	}
	state, err := gitops.ReadState(t.WorkDir)
	if err != nil {
		return err
	}
	if _, ok := stackNames(state, namespace)[stack]; !ok {
		return fmt.Errorf("%w: %s is not a stack of %s; list_stacks shows them", ErrInvalid, stack, namespace)
	}
	return nil
}

func (t *Tools) promQuery(ctx context.Context, q string) (string, error) {
	if t.Prometheus == "" {
		return "", errors.New("no Prometheus configured")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, t.Prometheus+"/api/v1/query?"+url.Values{"query": {q}}.Encode(), nil)
	if err != nil {
		return "", err
	}
	client := t.HTTP
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("prometheus: %w", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	var out struct {
		Status string `json:"status"`
		Error  string `json:"error"`
		Data   struct {
			ResultType string `json:"resultType"`
			Result     []struct {
				Metric map[string]string `json:"metric"`
				Value  []any             `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return "", fmt.Errorf("prometheus: %s", resp.Status)
	}
	if out.Status != "success" {
		return "", fmt.Errorf("%w: prometheus: %s", ErrInvalid, out.Error)
	}
	if len(out.Data.Result) == 0 {
		return "no data", nil
	}
	var b strings.Builder
	for i, r := range out.Data.Result {
		if i == 200 {
			b.WriteString("[more series]\n")
			break
		}
		labels := make([]string, 0, len(r.Metric))
		for k, v := range r.Metric {
			labels = append(labels, k+"="+v)
		}
		slices.Sort(labels)
		val := ""
		if len(r.Value) == 2 {
			val = fmt.Sprint(r.Value[1])
		}
		fmt.Fprintf(&b, "{%s} %s\n", strings.Join(labels, ", "), val)
	}
	return b.String(), nil
}

// Stack is one deployed stack.
type Stack struct {
	Name     string    `json:"name"`
	Commit   string    `json:"commit"`
	Services []Service `json:"services"`
}

type Service struct {
	Name     string `json:"name"`
	Replicas string `json:"replicas"`
	Image    string `json:"image"`
	Healthy  bool   `json:"healthy"`
}

// Stacks lists a namespace's deployed stacks with what Swarm runs for them.
func (t *Tools) Stacks(ctx context.Context, namespace string) ([]Stack, error) {
	if err := validOrg(namespace); err != nil {
		return nil, err
	}
	state, err := gitops.ReadState(t.WorkDir)
	if err != nil {
		return nil, err
	}
	out, err := t.Run(ctx, nil, nil, "docker", "service", "ls", "--format", "{{.Name}}\t{{.Replicas}}\t{{.Image}}")
	if err != nil {
		return nil, err
	}
	byStack := map[string][]Service{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		f := strings.Split(line, "\t")
		if len(f) != 3 {
			continue
		}
		stack, _, _ := strings.Cut(f[0], "_")
		running, desired, _ := strings.Cut(strings.Fields(f[1])[0], "/")
		byStack[stack] = append(byStack[stack], Service{Name: f[0], Replicas: f[1], Image: f[2], Healthy: running == desired && desired != "0"})
	}
	names := stackNames(state, namespace)
	var stacks []Stack
	for _, name := range sortedKeys(names) {
		stacks = append(stacks, Stack{Name: name, Commit: names[name], Services: byStack[name]})
	}
	return stacks, nil
}

func firstLines(s string, n int) string {
	lines := strings.SplitN(s, "\n", n+1)
	if len(lines) > n {
		lines = append(lines[:n], "[more]")
	}
	return strings.Join(lines, "\n")
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
