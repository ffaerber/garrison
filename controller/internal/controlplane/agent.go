package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"homecloud/controller/internal/apply"
	"homecloud/controller/internal/gitops"
)

// Message is one turn of an OpenAI-compatible chat.
type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

type ToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// LLM speaks the OpenAI-compatible chat completions API, as the spec asks of
// agents, e.g. to Ollama.
type LLM struct {
	BaseURL string // e.g. https://ollama.com/v1
	Model   string
	KeyFile string // read on every call, so a new key needs no restart
	HTTP    *http.Client
}

var ErrNoKey = errors.New("no LLM API key is configured; an admin puts it in /etc/homecloud/llm-api-key")

func (l *LLM) Chat(ctx context.Context, msgs []Message, tools []map[string]any) (Message, error) {
	key, err := os.ReadFile(l.KeyFile)
	if err != nil || len(bytes.TrimSpace(key)) == 0 {
		return Message{}, ErrNoKey
	}
	body, err := json.Marshal(map[string]any{"model": l.Model, "messages": msgs, "tools": tools})
	if err != nil {
		return Message{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(l.BaseURL, "/")+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return Message{}, err
	}
	req.Header.Set("Authorization", "Bearer "+string(bytes.TrimSpace(key)))
	req.Header.Set("Content-Type", "application/json")
	client := l.HTTP
	if client == nil {
		client = &http.Client{Timeout: 3 * time.Minute}
	}
	resp, err := client.Do(req)
	if err != nil {
		return Message{}, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode != http.StatusOK {
		return Message{}, fmt.Errorf("LLM: %s: %.300s", resp.Status, data)
	}
	var out struct {
		Choices []struct {
			Message Message `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return Message{}, fmt.Errorf("LLM: %w", err)
	}
	if len(out.Choices) == 0 {
		return Message{}, errors.New("LLM: no answer")
	}
	m := out.Choices[0].Message
	m.Role = "assistant"
	return m, nil
}

// Person is who signed in.
type Person struct {
	Name  string `json:"name"`
	Email string `json:"email"`
	Admin bool   `json:"admin"` // member of root-admins
}

// Step is one tool call the agent made, shown to the person.
type Step struct {
	Tool   string `json:"tool"`
	Args   string `json:"args"`
	Result string `json:"result"`
	Error  bool   `json:"error,omitempty"`
}

type Chatter interface {
	Chat(ctx context.Context, msgs []Message, tools []map[string]any) (Message, error)
}

// Agent answers questions about the system and proposes changes. It acts with
// the rights of the person it talks to: reads and proposals go to Gitea as
// that person, and changes reach the cluster only when someone allowed to
// approves them.
type Agent struct {
	LLM     Chatter
	Gitea   *Gitea
	Run     apply.Runner // docker CLI
	WorkDir string       // the controller's state, read-only
	Domain  string
}

const maxRounds = 10
const maxToolOutput = 8000

func (a *Agent) systemPrompt(p Person) string {
	role := "a regular user: they may look at everything but change nothing in the system organisation"
	if p.Admin {
		role = "a root admin: they may change the platform"
	}
	return fmt.Sprintf(`You are the operator agent of a home cloud at %[1]s. You answer questions about the running system and propose changes to it.

How the system works:
- It runs on Docker Swarm. The namespace "system" is a Gitea organisation; each of its repos that holds a compose.yaml is one Swarm stack, named after the repo.
- The controller deploys the main branch of every such repo. A change reaches the cluster only through a commit on main.
- Hostnames are <name>.%[1]s. People sign in through Keycloak.

You are talking to %[2]s, who is %[3]s. You act with exactly their rights.

Rules:
- Use the tools to look things up; do not guess. Say what you found and where.
- To change anything, use propose_change. It opens a pull request as the person; nothing changes until someone allowed approves it in the control plane. Never claim a change is made or deployed when you only proposed it.
- Propose whole files: read the current file first and change only what is needed.
- If a tool reports that something is not allowed, tell the person plainly; do not try to work around it.
- Secret values are never readable, by you or anyone. Refer to secrets by name.
- Be brief.`, a.Domain, p.Name, role)
}

func tool(name, desc string, props map[string]any, required ...string) map[string]any {
	if props == nil {
		props = map[string]any{}
	}
	if required == nil {
		required = []string{}
	}
	return map[string]any{"type": "function", "function": map[string]any{
		"name": name, "description": desc,
		"parameters": map[string]any{"type": "object", "properties": props, "required": required},
	}}
}

func str(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }

func (a *Agent) tools() []map[string]any {
	return []map[string]any{
		tool("list_stacks", "Lists the stacks the controller deployed, with the commit of each repo, and every Swarm service with its running/desired replicas and image.", nil),
		tool("stack_status", "Shows the tasks of one stack: their state and any error, newest first.", map[string]any{"stack": str("stack name, e.g. grafana")}, "stack"),
		tool("service_logs", "Shows the last lines of a service's logs.", map[string]any{
			"service": str("Swarm service name, <stack>_<service>, e.g. grafana_grafana"),
			"lines":   map[string]any{"type": "integer", "description": "how many lines, at most 300"},
		}, "service"),
		tool("list_repos", "Lists the repos of the system organisation.", nil),
		tool("list_files", "Lists a directory of a repo's main branch.", map[string]any{"repo": str("repo name"), "path": str("directory, empty for the top")}, "repo"),
		tool("read_file", "Reads a file of a repo's main branch.", map[string]any{"repo": str("repo name"), "path": str("file path, e.g. compose.yaml")}, "repo", "path"),
		tool("propose_change", "Proposes a new version of one file as a pull request, as the person. Nothing is changed until it is approved.", map[string]any{
			"repo":        str("repo name"),
			"path":        str("file path"),
			"content":     str("the complete new content of the file"),
			"title":       str("short title of the change"),
			"description": str("why, and what it does"),
		}, "repo", "path", "content", "title"),
	}
}

// Ask runs one turn: the person's input, tool calls as needed, the answer.
// history holds earlier turns without the system prompt.
func (a *Agent) Ask(ctx context.Context, p Person, history []Message, input string) (string, []Step, []Message, error) {
	msgs := append([]Message{{Role: "system", Content: a.systemPrompt(p)}}, history...)
	msgs = append(msgs, Message{Role: "user", Content: input})
	var steps []Step
	for round := 0; round < maxRounds; round++ {
		reply, err := a.LLM.Chat(ctx, msgs, a.tools())
		if err != nil {
			return "", steps, history, err
		}
		msgs = append(msgs, reply)
		if len(reply.ToolCalls) == 0 {
			return reply.Content, steps, msgs[1:], nil
		}
		for _, call := range reply.ToolCalls {
			out, err := a.call(ctx, p, call.Function.Name, call.Function.Arguments)
			step := Step{Tool: call.Function.Name, Args: call.Function.Arguments, Result: out}
			if err != nil {
				step.Error = true
				step.Result = err.Error()
				if errors.Is(err, ErrForbidden) {
					step.Result = fmt.Sprintf("not allowed: %s may not do this", p.Name)
				}
				out = "error: " + step.Result
			}
			if len(out) > maxToolOutput {
				out = out[:maxToolOutput] + "\n[truncated]"
			}
			if len(step.Result) > 2000 {
				step.Result = step.Result[:2000] + "\n[truncated]"
			}
			steps = append(steps, step)
			msgs = append(msgs, Message{Role: "tool", ToolCallID: call.ID, Content: out})
		}
	}
	return "I stopped after too many steps without an answer. Please ask more narrowly.", steps, msgs[1:], nil
}

var swarmName = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,100}$`)

func (a *Agent) call(ctx context.Context, p Person, name, rawArgs string) (string, error) {
	var args struct {
		Stack, Service, Repo, Path, Content, Title, Description string
		Lines                                                   int
	}
	if strings.TrimSpace(rawArgs) != "" {
		if err := json.Unmarshal([]byte(rawArgs), &args); err != nil {
			return "", fmt.Errorf("arguments are not valid JSON: %w", err)
		}
	}
	switch name {
	case "list_stacks":
		return a.listStacks(ctx)
	case "stack_status":
		if !swarmName.MatchString(args.Stack) {
			return "", fmt.Errorf("invalid stack name %q", args.Stack)
		}
		out, err := a.Run(ctx, nil, nil, "docker", "stack", "ps", "--no-trunc", "--format", "{{.Name}}\t{{.CurrentState}}\t{{.Error}}", args.Stack)
		return firstLines(string(out), 30), err
	case "service_logs":
		if !swarmName.MatchString(args.Service) {
			return "", fmt.Errorf("invalid service name %q", args.Service)
		}
		n := args.Lines
		if n <= 0 || n > 300 {
			n = 100
		}
		// Container output arrives on both streams; the names go in as arguments, never into the script.
		out, err := a.Run(ctx, nil, nil, "sh", "-c", `docker service logs --tail "$1" --no-task-ids --timestamps "$2" 2>&1`, "sh", fmt.Sprint(n), args.Service)
		return string(out), err
	case "list_repos":
		repos, err := a.Gitea.Repos(ctx, p.Name)
		if err != nil {
			return "", err
		}
		var b strings.Builder
		for _, r := range repos {
			fmt.Fprintf(&b, "%s\t%s\n", r.Name, r.Description)
		}
		return b.String(), nil
	case "list_files":
		entries, err := a.Gitea.List(ctx, p.Name, args.Repo, args.Path)
		if err != nil {
			return "", err
		}
		var b strings.Builder
		for _, e := range entries {
			fmt.Fprintf(&b, "%s\t%s\n", e.Type, e.Path)
		}
		return b.String(), nil
	case "read_file":
		return a.Gitea.Read(ctx, p.Name, args.Repo, args.Path)
	case "propose_change":
		c, err := a.Gitea.Propose(ctx, p.Name, args.Repo, args.Path, args.Content, args.Title, args.Description)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("Opened change %s#%d: %s. It waits for approval in the control plane.", c.Repo, c.Number, c.URL), nil
	}
	return "", fmt.Errorf("unknown tool %q", name)
}

// Stack is one deployed stack as the control plane shows it.
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

// Stacks combines the controller's state with what Swarm runs.
func (a *Agent) Stacks(ctx context.Context) ([]Stack, error) {
	state, err := gitops.ReadState(a.WorkDir)
	if err != nil {
		return nil, err
	}
	out, err := a.Run(ctx, nil, nil, "docker", "service", "ls", "--format", "{{.Name}}\t{{.Replicas}}\t{{.Image}}")
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
	var stacks []Stack
	seen := map[string]bool{}
	for _, name := range state.Names() {
		stacks = append(stacks, Stack{Name: name, Commit: state.Stacks[name], Services: byStack[name]})
		seen[name] = true
	}
	// Stacks Swarm runs that the controller did not deploy, e.g. app stacks.
	var others []string
	for name := range byStack {
		if !seen[name] {
			others = append(others, name)
		}
	}
	slices.Sort(others)
	for _, name := range others {
		stacks = append(stacks, Stack{Name: name, Services: byStack[name]})
	}
	return stacks, nil
}

func (a *Agent) listStacks(ctx context.Context) (string, error) {
	stacks, err := a.Stacks(ctx)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for _, s := range stacks {
		commit := s.Commit
		if len(commit) > 12 {
			commit = commit[:12]
		}
		if commit == "" {
			commit = "not deployed by the controller"
		}
		fmt.Fprintf(&b, "%s (%s)\n", s.Name, commit)
		for _, svc := range s.Services {
			fmt.Fprintf(&b, "  %s %s %s\n", svc.Name, svc.Replicas, svc.Image)
		}
	}
	return b.String(), nil
}

func firstLines(s string, n int) string {
	lines := strings.SplitN(s, "\n", n+1)
	if len(lines) > n {
		lines = append(lines[:n], "[more]")
	}
	return strings.Join(lines, "\n")
}
