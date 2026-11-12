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

	"homecloud/control-plane/internal/apply"
	"homecloud/control-plane/internal/gitops"
)

// Person is who is asking: the signed-in user behind a tool call.
type Person struct {
	Name  string `json:"name"`
	Email string `json:"email"`
	Admin bool   `json:"admin"` // member of root-admins
}

// SystemNamespace is the namespace the deployer deploys.
const SystemNamespace = "system"

// Tools are what the operator agents may do. Reads of the cluster (stacks,
// logs, metrics) are open to everyone signed in, as everything is; reads and
// proposals in Gitea go as the person, so Gitea applies their own rights.
// Nothing here can merge or deploy: a person merges in Gitea.
type Tools struct {
	Gitea      *Gitea
	Run        apply.Runner // docker CLI
	WorkDir    string       // the deployer's state, read-only
	Prometheus string       // e.g. http://prometheus_prometheus:9090
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

func (t *Tools) Specs() []Spec {
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
	}
}

var swarmName = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,100}$`)

type args struct {
	Namespace, Stack, Service, Repo, Path, Content, Title, Description, Query string
	Lines                                                                     int
}

// Call runs one tool for person.
func (t *Tools) Call(ctx context.Context, p Person, name string, raw json.RawMessage) (string, error) {
	var a args
	if len(raw) > 0 && string(raw) != "null" {
		if err := json.Unmarshal(raw, &a); err != nil {
			return "", fmt.Errorf("%w: arguments: %v", ErrInvalid, err)
		}
	}
	out, err := t.call(ctx, p, name, a)
	if len(out) > maxToolOutput {
		out = out[:maxToolOutput] + "\n[truncated]"
	}
	return out, err
}

func (t *Tools) call(ctx context.Context, p Person, name string, a args) (string, error) {
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
	case "propose_change":
		c, err := t.Gitea.Propose(ctx, p.Name, a.Namespace, a.Repo, a.Path, a.Content, a.Title, a.Description)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("Opened change %s/%s#%d. Nothing is changed yet: a person allowed to merge must review and merge it in Gitea.\nEnd your answer with this link, exactly as it is, so the person can open it:\n%s", c.Namespace, c.Repo, c.Number, c.URL), nil
	}
	return "", fmt.Errorf("%w: unknown tool %q", ErrInvalid, name)
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
