package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"garrison/control-plane/internal/agents"
)

// fakeGitea answers like Gitea: alice may write to garrison, bob may only read.
type fakeGitea struct {
	mu      sync.Mutex
	prs     int
	sudo    []string
	heads   []string         // branches of open pull requests
	patches []map[string]any // diffpatch requests
}

func (f *fakeGitea) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if u, p, ok := r.BasicAuth(); !ok || u != "admin" || p != "pw" {
		http.Error(w, `{"message":"unauthorized"}`, http.StatusUnauthorized)
		return
	}
	as := r.Header.Get("Sudo")
	f.sudo = append(f.sudo, as)
	if r.Method != http.MethodGet && as != "alice" {
		http.Error(w, `{"message":"forbidden"}`, http.StatusForbidden)
		return
	}
	p := strings.TrimPrefix(r.URL.Path, "/api/v1")
	switch {
	case p == "/orgs":
		io.WriteString(w, `[{"username":"garrison","description":"the platform"},{"username":"homepage"}]`)
	case p == "/orgs/garrison" || p == "/orgs/homepage":
		io.WriteString(w, "{}")
	case p == "/orgs/garrison/repos":
		json.NewEncoder(w).Encode([]Repo{{Name: "grafana"}})
	case p == "/repos/garrison/grafana" && r.Method == http.MethodGet:
		fmt.Fprintf(w, `{"permissions":{"push":%v}}`, as == "alice")
	case p == "/repos/garrison/grafana/raw/compose.yaml":
		io.WriteString(w, "services: {}\n")
	case p == "/repos/garrison/grafana/contents/compose.yaml" && r.Method == http.MethodGet:
		io.WriteString(w, `{"sha":"abc"}`)
	case p == "/repos/garrison/grafana/contents/compose.yaml":
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if body["branch"] != "main" || body["new_branch"] == "" {
			http.Error(w, `{"message":"must write to a new branch"}`, http.StatusBadRequest)
			return
		}
		io.WriteString(w, "{}")
	case p == "/repos/garrison/grafana/diffpatch":
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		f.patches = append(f.patches, body)
		io.WriteString(w, "{}")
	case p == "/repos/garrison/grafana/pulls" && r.Method == http.MethodPost:
		var body struct{ Head string }
		json.NewDecoder(r.Body).Decode(&body)
		f.prs++
		f.heads = append(f.heads, body.Head)
		io.WriteString(w, `{"number":7,"html_url":"https://git.example.com/garrison/grafana/pulls/7"}`)
	case p == "/repos/garrison/grafana/pulls":
		var out []map[string]any
		for _, h := range f.heads {
			out = append(out, map[string]any{"number": 7, "html_url": "https://git.example.com/garrison/grafana/pulls/7", "head": map[string]any{"ref": h}})
		}
		json.NewEncoder(w).Encode(out)
	case strings.HasSuffix(p, "/pulls"):
		io.WriteString(w, "[]")
	case p == "/users/alice":
		io.WriteString(w, `{"email":"alice@example.com"}`)
	case p == "/repos/homepage/site" && as == "":
		io.WriteString(w, `{"private":false}`)
	case p == "/repos/homepage/secret" && as == "":
		io.WriteString(w, `{"private":true}`)
	case p == "/repos/homepage/config/raw/members.yaml":
		io.WriteString(w, "admins: [alice]\nmembers: [bob]\n")
	// homepage/config has two agents, one of them broken, and a skill.
	case p == "/repos/homepage/config/contents/agents":
		io.WriteString(w, `[{"name":"marketing","type":"dir"},{"name":"broken","type":"dir"},{"name":"README.md","type":"file"}]`)
	case p == "/repos/homepage/config/raw/agents/marketing/agent.yaml":
		io.WriteString(w, "description: Media\ntools: [read_file]\n")
	case p == "/repos/homepage/config/raw/agents/marketing/prompt.md":
		io.WriteString(w, "You make media.\n")
	case p == "/repos/homepage/config/raw/agents/broken/agent.yaml":
		io.WriteString(w, "tools: [rm_rf]\n")
	case p == "/repos/homepage/config/raw/agents/broken/prompt.md":
		io.WriteString(w, "x\n")
	case p == "/repos/homepage/config/contents/agents/marketing/skills/thumbnail":
		io.WriteString(w, `[{"name":"SKILL.md","type":"file"},{"name":"frame.svg","type":"file"}]`)
	case p == "/repos/homepage/config/raw/agents/marketing/skills/thumbnail/SKILL.md":
		io.WriteString(w, "---\nname: thumbnail\ndescription: Make a thumbnail.\n---\nUse frame.svg.\n")
	case p == "/repos/homepage/config/raw/agents/marketing/skills/thumbnail/frame.svg":
		io.WriteString(w, "<svg/>")
	default:
		http.NotFound(w, r)
	}
}

type fakeVerifier map[string]Person

func (f fakeVerifier) Verify(_ context.Context, tok string) (Person, error) {
	if p, ok := f[tok]; ok {
		return p, nil
	}
	return Person{}, errors.New("bad token")
}

func setup(t *testing.T) (*MCP, *fakeGitea) {
	t.Helper()
	fg := &fakeGitea{}
	gsrv := httptest.NewServer(fg)
	t.Cleanup(gsrv.Close)
	prom := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/query" {
			http.NotFound(w, r)
			return
		}
		if r.URL.Query().Get("query") == "bad(" {
			io.WriteString(w, `{"status":"error","error":"parse error"}`)
			return
		}
		io.WriteString(w, `{"status":"success","data":{"resultType":"vector","result":[{"metric":{"container_label_com_docker_swarm_service_name":"grafana_grafana"},"value":[1,"12.5"]}]}}`)
	}))
	t.Cleanup(prom.Close)
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "applied.json"), []byte(`{"stacks":{"grafana":"0123456789abcdef"},"apps":{"homepage/todo@main":{"namespace":"homepage","repo":"todo","environment":"main","stack":"todo","commit":"fedcba9876543210"}}}`), 0o644)
	os.WriteFile(filepath.Join(dir, "snapshots.json"), []byte(`{"todo":{"namespace":"homepage","snapshots":[{"name":"deploy-20261113-150405-abc1234","created":"2026-11-13T15:04:05Z"}]}}`), 0o644)
	run := func(_ context.Context, _ []string, _ []byte, name string, args ...string) ([]byte, error) {
		if name == "docker" && len(args) > 1 && args[0] == "service" && args[1] == "ls" {
			return []byte("grafana_grafana\t1/1\tgrafana/grafana:12\nhello_web\t1/1\twhoami\ntodo_web\t1/1\tgit.example.com/homepage/todo:fedcba987654\n"), nil
		}
		return []byte("log line\n"), nil
	}
	tools := &Tools{Gitea: &Gitea{URL: gsrv.URL, User: "admin", Password: "pw"}, Run: run, WorkDir: dir, Prometheus: prom.URL}
	return &MCP{Tools: tools, Verifier: fakeVerifier{"tok-alice": {Name: "alice", Admin: true}, "tok-bob": {Name: "bob"}, "tok-carol": {Name: "carol"}}}, fg
}

// rpc posts one JSON-RPC request with a bearer token and returns status and decoded body.
func rpc(t *testing.T, m *MCP, token, method string, params any) (int, map[string]any) {
	t.Helper()
	return rpcAt(t, m, "/mcp", token, method, params)
}

func rpcAt(t *testing.T, m *MCP, path, token, method string, params any) (int, map[string]any) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(body)))
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	m.ServeHTTP(w, r)
	var out map[string]any
	json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

func callText(t *testing.T, m *MCP, token, tool string, args map[string]any) (string, bool) {
	t.Helper()
	code, out := rpc(t, m, token, "tools/call", map[string]any{"name": tool, "arguments": args})
	if code != http.StatusOK || out["result"] == nil {
		t.Fatalf("%s: %d %v", tool, code, out)
	}
	res := out["result"].(map[string]any)
	text := res["content"].([]any)[0].(map[string]any)["text"].(string)
	isErr, _ := res["isError"].(bool)
	return text, isErr
}

func TestMCPNeedsAToken(t *testing.T) {
	m, _ := setup(t)
	if code, _ := rpc(t, m, "", "tools/list", nil); code != http.StatusUnauthorized {
		t.Errorf("no token: %d", code)
	}
	if code, _ := rpc(t, m, "forged", "tools/list", nil); code != http.StatusUnauthorized {
		t.Errorf("forged token: %d", code)
	}
	r := httptest.NewRequest(http.MethodGet, "/mcp", nil)
	w := httptest.NewRecorder()
	m.ServeHTTP(w, r)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET: %d", w.Code)
	}
}

func TestMCPHandshake(t *testing.T) {
	m, _ := setup(t)
	_, out := rpc(t, m, "tok-bob", "initialize", map[string]any{"protocolVersion": "2025-03-26"})
	res := out["result"].(map[string]any)
	if res["protocolVersion"] != "2025-03-26" || res["capabilities"].(map[string]any)["tools"] == nil {
		t.Errorf("initialize = %v", res)
	}
	_, out = rpc(t, m, "tok-bob", "tools/list", nil)
	var names []string
	for _, tl := range out["result"].(map[string]any)["tools"].([]any) {
		names = append(names, tl.(map[string]any)["name"].(string))
	}
	got := strings.Join(names, " ")
	for _, want := range []string{"service_logs", "service_usage", "query_metrics", "propose_change", "list_namespaces"} {
		if !strings.Contains(got, want) {
			t.Errorf("tools/list lacks %s: %s", want, got)
		}
	}
	for _, never := range []string{"merge", "approve", "deploy"} {
		if strings.Contains(got, never) {
			t.Errorf("tools/list offers %s: %s", never, got)
		}
	}
	// A notification gets no body.
	body := `{"jsonrpc":"2.0","method":"notifications/initialized"}`
	r := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer tok-bob")
	w := httptest.NewRecorder()
	m.ServeHTTP(w, r)
	if w.Code != http.StatusAccepted {
		t.Errorf("notification: %d", w.Code)
	}
	if code, out := rpc(t, m, "tok-bob", "nope", nil); code != 200 || out["error"] == nil {
		t.Errorf("unknown method: %d %v", code, out)
	}
}

func TestEveryoneMayLook(t *testing.T) {
	m, _ := setup(t)
	text, isErr := callText(t, m, "tok-bob", "list_namespaces", nil)
	if isErr || !strings.Contains(text, "garrison") || !strings.Contains(text, "homepage") {
		t.Errorf("list_namespaces = %q", text)
	}
	text, isErr = callText(t, m, "tok-bob", "list_stacks", map[string]any{"namespace": "garrison"})
	if isErr || !strings.Contains(text, "grafana (commit 0123456789ab)") || strings.Contains(text, "hello") {
		t.Errorf("list_stacks = %q", text)
	}
	text, isErr = callText(t, m, "tok-bob", "list_stacks", map[string]any{"namespace": "homepage"})
	if isErr || !strings.Contains(text, "todo (commit fedcba987654)") || strings.Contains(text, "grafana") {
		t.Errorf("list_stacks homepage = %q", text)
	}
	text, isErr = callText(t, m, "tok-bob", "list_snapshots", map[string]any{"namespace": "homepage", "stack": "todo"})
	if isErr || !strings.Contains(text, "deploy-20261113-150405-abc1234\t2026-11-13T15:04:05Z") {
		t.Errorf("list_snapshots = %q", text)
	}
	if text, isErr = callText(t, m, "tok-bob", "list_snapshots", map[string]any{"namespace": "garrison", "stack": "todo"}); !isErr {
		t.Errorf("list_snapshots of another namespace's stack = %q", text)
	}
	if text, isErr = callText(t, m, "tok-bob", "service_logs", map[string]any{"namespace": "homepage", "service": "todo_web"}); isErr {
		t.Errorf("service_logs of an app = %q", text)
	}
	if text, isErr = callText(t, m, "tok-bob", "service_logs", map[string]any{"namespace": "garrison", "service": "grafana_grafana"}); isErr || text != "log line\n" {
		t.Errorf("service_logs = %q", text)
	}
	if text, isErr = callText(t, m, "tok-bob", "service_usage", map[string]any{"namespace": "garrison", "stack": "grafana"}); isErr || !strings.Contains(text, "12.5") {
		t.Errorf("service_usage = %q", text)
	}
	if text, isErr = callText(t, m, "tok-bob", "query_metrics", map[string]any{"query": "up"}); isErr || !strings.Contains(text, "grafana_grafana") {
		t.Errorf("query_metrics = %q", text)
	}
	if text, isErr = callText(t, m, "tok-bob", "read_file", map[string]any{"namespace": "garrison", "repo": "grafana", "path": "compose.yaml"}); isErr || text != "services: {}\n" {
		t.Errorf("read_file = %q", text)
	}
}

func TestToolsStayInTheirNamespace(t *testing.T) {
	m, _ := setup(t)
	for _, c := range []struct {
		tool string
		args map[string]any
	}{
		{"service_logs", map[string]any{"namespace": "garrison", "service": "hello_web"}},            // not a garrison stack
		{"service_logs", map[string]any{"namespace": "garrison", "service": "grafana_grafana; id"}},  // not a name
		{"stack_status", map[string]any{"namespace": "homepage", "stack": "grafana"}},                // wrong namespace
		{"read_file", map[string]any{"namespace": "garrison", "repo": "grafana", "path": "../../x"}}, // outside the repo
		{"read_file", map[string]any{"namespace": "../admin", "repo": "grafana", "path": "a"}},       // not a namespace
		{"query_metrics", map[string]any{"query": "bad("}},                                           // Prometheus refuses
	} {
		if text, isErr := callText(t, m, "tok-alice", c.tool, c.args); !isErr {
			t.Errorf("%s %v accepted: %q", c.tool, c.args, text)
		}
	}
}

func TestProposalsActAsThePerson(t *testing.T) {
	args := map[string]any{"namespace": "garrison", "repo": "grafana", "path": "compose.yaml", "content": "services: {x: {}}\n", "title": "Add x"}
	m, fg := setup(t)
	text, isErr := callText(t, m, "tok-bob", "propose_change", args)
	if !isErr || !strings.Contains(text, "not allowed: bob") || fg.prs != 0 {
		t.Errorf("bob: %q, prs %d", text, fg.prs)
	}
	text, isErr = callText(t, m, "tok-alice", "propose_change", args)
	if isErr || fg.prs != 1 || !strings.HasSuffix(text, "\nhttps://git.example.com/garrison/grafana/pulls/7") || !strings.Contains(text, "Nothing is changed yet") {
		t.Errorf("alice: %q, prs %d", text, fg.prs)
	}
	for _, s := range fg.sudo {
		if s != "alice" && s != "bob" {
			t.Errorf("Gitea called as %q, not as the person", s)
		}
	}
}

func TestPatchesActAsThePersonAndAddUp(t *testing.T) {
	m, fg := setup(t)
	args := map[string]any{"namespace": "garrison", "repo": "grafana", "patch": "diff --git a/x b/x\n", "title": "Add x"}
	if text, isErr := callText(t, m, "tok-bob", "propose_patch", args); !isErr || !strings.Contains(text, "not allowed") || len(fg.patches) != 0 {
		t.Fatalf("bob: %q", text)
	}
	text, isErr := callText(t, m, "tok-alice", "propose_patch", args)
	if isErr || fg.prs != 1 || !strings.HasSuffix(text, "/pulls/7") {
		t.Fatalf("alice: %q", text)
	}
	first := fg.patches[0]
	branch, _ := first["new_branch"].(string)
	if first["branch"] != "main" || !strings.HasPrefix(branch, "change/alice-") || !strings.Contains(text, "Branch: "+branch+"\n") {
		t.Fatalf("first patch %v; %q", first, text)
	}
	// More on the same branch: no new pull request.
	args["branch"] = branch
	if text, isErr := callText(t, m, "tok-alice", "propose_patch", args); isErr || fg.prs != 1 || fg.patches[1]["branch"] != branch || fg.patches[1]["new_branch"] != nil {
		t.Fatalf("follow-up: %q, prs %d, %v", text, fg.prs, fg.patches[1])
	}
	// Never onto main or any branch that is not a change's.
	for _, b := range []string{"main", "dev", "change/../main"} {
		args["branch"] = b
		if text, isErr := callText(t, m, "tok-alice", "propose_patch", args); !isErr || len(fg.patches) != 2 {
			t.Errorf("branch %s: %q", b, text)
		}
	}
	args["branch"] = ""
	args["patch"] = " "
	if _, isErr := callText(t, m, "tok-alice", "propose_patch", args); !isErr {
		t.Error("an empty patch was proposed")
	}
}

func TestOpenWebUISync(t *testing.T) {
	fg := &fakeGitea{}
	gsrv := httptest.NewServer(fg)
	defer gsrv.Close()
	var mu sync.Mutex
	preset := "gpt-oss:120b"
	models := map[string]owuiModel{
		"agent-gone": {ID: "agent-gone", Name: "agent-gone"}, "ns-garrison": {ID: "ns-garrison", Name: "garrison"}, "other": {ID: "other"},
		"homepage/old": {ID: "homepage/old", Name: "homepage/old", Meta: map[string]any{AgentMarker: "homepage/old"}},
		"homepage/marketing": {ID: "homepage/marketing", Name: "homepage/marketing", BaseModelID: &preset, IsActive: true,
			Meta: map[string]any{AgentMarker: "homepage/marketing", "description": "Media"}},
	}
	channels := map[string]owuiChannel{
		"c-gone":  {ID: "c-gone", Name: "gone", Meta: map[string]any{ChannelMarker: "gone"}},
		"c-other": {ID: "c-other", Name: "random"},
		"c-team":  {ID: "c-team", Name: "homepage-team", Meta: map[string]any{legacyTeamMarker: "homepage"}},
	}
	nextChannel := 0
	profileName := "Installer"
	messages := map[string][]owuiMessage{} // channel id -> messages
	var calls []string
	var groups []owuiGroup
	groupUsers := map[string][]string{}
	ow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, r.Method+" "+r.URL.Path)
		if r.URL.Path == "/api/v1/auths/signin" {
			io.WriteString(w, `{"token":"t"}`)
			return
		}
		if r.Header.Get("Authorization") != "Bearer t" {
			http.Error(w, "unauthorized", 401)
			return
		}
		switch r.URL.Path {
		case "/api/v1/models/all":
			var list []owuiModel
			for _, m := range models {
				list = append(list, m)
			}
			json.NewEncoder(w).Encode(list)
		case "/api/v1/models/create":
			var m owuiModel
			json.NewDecoder(r.Body).Decode(&m)
			models[m.ID] = m
			io.WriteString(w, "{}")
		case "/api/v1/models/model/update":
			// As Open WebUI: the update keeps the access grants; only
			// model/access/update changes them.
			var m owuiModel
			json.NewDecoder(r.Body).Decode(&m)
			m.AccessGrants = models[m.ID].AccessGrants
			models[m.ID] = m
			io.WriteString(w, "{}")
		case "/api/v1/models/model/access/update":
			var b struct {
				ID           string `json:"id"`
				AccessGrants []any  `json:"access_grants"`
			}
			json.NewDecoder(r.Body).Decode(&b)
			m := models[b.ID]
			m.AccessGrants = b.AccessGrants
			models[b.ID] = m
			io.WriteString(w, "{}")
		case "/api/v1/models/model/delete":
			var b map[string]string
			json.NewDecoder(r.Body).Decode(&b)
			delete(models, b["id"])
			io.WriteString(w, "true")
		case "/api/v1/groups/":
			json.NewEncoder(w).Encode(groups)
		case "/api/v1/groups/create":
			var g owuiGroup
			json.NewDecoder(r.Body).Decode(&g)
			g.ID = "g" + g.Name
			groups = append(groups, g)
			json.NewEncoder(w).Encode(g)
		case "/api/v1/users/all":
			io.WriteString(w, `{"users":[{"id":"u-alice","email":"Alice@example.com"},{"id":"u-carol","email":"carol@example.com"}]}`)
		case "/api/v1/auths/":
			json.NewEncoder(w).Encode(map[string]string{"id": "cp", "name": profileName})
		case "/api/v1/auths/update/profile":
			var b map[string]string
			json.NewDecoder(r.Body).Decode(&b)
			profileName = b["name"]
			io.WriteString(w, "{}")
		case "/api/v1/channels/list":
			var list []owuiChannel
			for _, c := range channels {
				list = append(list, c)
			}
			json.NewEncoder(w).Encode(list)
		case "/api/v1/channels/create":
			var c owuiChannel
			json.NewDecoder(r.Body).Decode(&c)
			nextChannel++
			c.ID = "c" + string(rune('0'+nextChannel))
			channels[c.ID] = c
			json.NewEncoder(w).Encode(c)
		default:
			if id, ok := strings.CutPrefix(r.URL.Path, "/api/v1/groups/id/"); ok {
				id, op, _ := strings.Cut(id, "/")
				var b struct {
					UserIDs []string `json:"user_ids"`
				}
				json.NewDecoder(r.Body).Decode(&b)
				switch op {
				case "users":
					out := []map[string]string{}
					for _, u := range groupUsers[id] {
						out = append(out, map[string]string{"id": u})
					}
					json.NewEncoder(w).Encode(out)
				case "users/add":
					groupUsers[id] = append(groupUsers[id], b.UserIDs...)
					io.WriteString(w, "{}")
				case "users/remove":
					groupUsers[id] = slices.DeleteFunc(groupUsers[id], func(u string) bool { return slices.Contains(b.UserIDs, u) })
					io.WriteString(w, "{}")
				default:
					http.NotFound(w, r)
				}
				return
			}
			rest := strings.TrimPrefix(r.URL.Path, "/api/v1/channels/")
			parts := strings.Split(rest, "/")
			if len(parts) >= 2 && parts[1] == "messages" {
				ch := parts[0]
				switch {
				case len(parts) == 3 && parts[2] == "pinned":
					var out []owuiMessage
					for _, m := range messages[ch] {
						if m.IsPinned {
							out = append(out, m)
						}
					}
					json.NewEncoder(w).Encode(out)
				case len(parts) == 2:
					json.NewEncoder(w).Encode(messages[ch])
				case len(parts) == 3 && parts[2] == "post":
					var b map[string]string
					json.NewDecoder(r.Body).Decode(&b)
					m := owuiMessage{ID: "m" + string(rune('0'+len(messages[ch]))), UserID: "cp", Content: b["content"]}
					messages[ch] = append(messages[ch], m)
					json.NewEncoder(w).Encode(m)
				case len(parts) == 4 && parts[3] == "pin":
					for i := range messages[ch] {
						if messages[ch][i].ID == parts[2] {
							messages[ch][i].IsPinned = true
						}
					}
					io.WriteString(w, "{}")
				default:
					http.NotFound(w, r)
				}
				return
			}
			if id, ok := strings.CutSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/channels/"), "/delete"); ok && r.Method == http.MethodDelete {
				delete(channels, id)
				io.WriteString(w, "true")
				return
			}
			if id, ok := strings.CutSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/channels/"), "/update"); ok {
				var c owuiChannel
				json.NewDecoder(r.Body).Decode(&c)
				c.ID = id
				channels[id] = c
				io.WriteString(w, "{}")
				return
			}
			if strings.HasSuffix(r.URL.Path, "/access/update") {
				io.WriteString(w, "{}")
				return
			}
			http.NotFound(w, r)
		}
	}))
	defer ow.Close()
	catalog := &Catalog{}
	o := &OpenWebUI{URL: ow.URL, Email: "e", Password: "p", Domain: "example.com",
		Gitea: &Gitea{URL: gsrv.URL, User: "admin", Password: "pw"}, Agents: catalog}
	if err := o.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Agents on public repos are for everyone; homepage's channel is for its
	// group, garrison's for everyone; the team channel of before is gone.
	gid := "g" + GroupPrefix + "homepage"
	if models["homepage/marketing"].Meta[AccessMarker] != "everyone" || models["garrison/operator"].Meta[AccessMarker] != "everyone" {
		t.Errorf("access: %v %v", models["homepage/marketing"].Meta, models["garrison/operator"].Meta)
	}
	for _, c := range channels {
		grants, _ := json.Marshal(c.AccessGrants)
		switch c.Meta[ChannelMarker] {
		case "homepage":
			if !strings.Contains(string(grants), `"principal_id":"`+gid+`"`) || strings.Contains(string(grants), `"*"`) {
				t.Errorf("homepage's channel is for %s", grants)
			}
		case "garrison":
			if c.AccessGrants != nil && !strings.Contains(string(grants), `"*"`) {
				t.Errorf("garrison's channel is for %s", grants)
			}
		}
		if c.Meta[legacyTeamMarker] != nil {
			t.Errorf("a team channel is left: %v", c)
		}
	}
	if strings.Join(groupUsers[gid], ",") != "u-alice" {
		t.Errorf("homepage's group: %v (alice is in it; bob has not signed in to Open WebUI; carol is not in homepage)", groupUsers[gid])
	}
	// garrison has no agents folder: its operator. homepage has its own agents,
	// of which the broken one is left out.
	sys, ok := models["garrison/operator"]
	if !ok || sys.Name != "garrison/operator" || models["homepage/marketing"].Name != "homepage/marketing" {
		t.Fatalf("models = %v", models)
	}
	for _, id := range []string{"homepage/operator", "homepage/broken"} {
		if _, ok := models[id]; ok {
			t.Errorf("%s was added", id)
		}
	}
	if _, ok := catalog.Get("homepage/marketing"); !ok {
		t.Error("the catalog lacks homepage/marketing")
	}
	if _, ok := models["homepage/old"]; ok {
		t.Error("the model of a removed agent folder was kept")
	}
	if _, ok := models["agent-gone"]; ok {
		t.Error("the agent of a deleted namespace was kept")
	}
	if _, ok := models["ns-garrison"]; ok {
		t.Error("an agent of an earlier version was kept")
	}
	if _, ok := models["other"]; !ok {
		t.Error("a model the sync does not own was removed")
	}
	// An agent is the agent service's model: an entry without a base model,
	// which everyone may use. The preset of an earlier version is made again.
	if sys.BaseModelID != nil || len(sys.AccessGrants) != 1 || sys.Meta[AgentMarker] != "garrison/operator" {
		t.Errorf("garrison/operator = %+v", sys)
	}
	if m := models["homepage/marketing"]; m.BaseModelID != nil || m.Meta["description"] != "Media" {
		t.Errorf("homepage/marketing = %+v", m)
	}
	if _, ok := models["gpt-oss:120b"]; ok {
		t.Error("an entry for the LLM was made")
	}
	// One channel per namespace, everyone may post, the gone one removed, others kept.
	byNS := map[string]owuiChannel{}
	for _, c := range channels {
		if ns, ok := c.Meta[ChannelMarker].(string); ok {
			byNS[ns] = c
		}
	}
	if len(byNS) != 2 || byNS["garrison"].Name != "garrison" || byNS["homepage"].Name != "homepage" {
		t.Errorf("channels = %v", channels)
	}
	if _, ok := channels["c-gone"]; ok {
		t.Error("the channel of a deleted namespace was kept")
	}
	if _, ok := channels["c-other"]; !ok {
		t.Error("a channel the sync does not own was removed")
	}
	// A renamed channel is put back.
	renamed := byNS["garrison"]
	renamed.Name = "renamed-by-an-admin"
	channels[renamed.ID] = renamed
	o.Sync(context.Background())
	if channels[renamed.ID].Name != "garrison" {
		t.Errorf("renamed channel not put back: %v", channels[renamed.ID])
	}
	// The gateway's account says it is not the agent.
	if profileName != AccountName {
		t.Errorf("account name = %q", profileName)
	}
	// Each namespace channel has the how-to pinned, once.
	sysID := byNS["garrison"].ID
	pinnedHowTos := func() int {
		n := 0
		for _, m := range messages[sysID] {
			if m.IsPinned && strings.HasPrefix(m.Content, HowToMarker) && strings.Contains(m.Content, "`@garrison/operator") {
				n++
			}
		}
		return n
	}
	if pinnedHowTos() != 1 {
		t.Errorf("garrison channel messages = %+v", messages[sysID])
	}
	// Someone unpins it: it is pinned again, not posted twice.
	messages[sysID][0].IsPinned = false
	o.Sync(context.Background())
	if pinnedHowTos() != 1 || len(messages[sysID]) != 1 {
		t.Errorf("after unpinning: %+v", messages[sysID])
	}
	// A second sync changes nothing.
	calls = nil
	o.Sync(context.Background())
	for _, c := range calls {
		if strings.HasSuffix(c, "/signin") {
			t.Error("second sync signed in again")
		}
		if strings.Contains(c, "create") || strings.Contains(c, "update") || strings.Contains(c, "delete") || strings.HasSuffix(c, "/post") || strings.HasSuffix(c, "/pin") {
			t.Errorf("second sync called %s", c)
		}
	}
}

func TestAgentEndpointsServeOnlyTheirTools(t *testing.T) {
	m, fg := setup(t)
	m.Agents = &Catalog{}
	m.Agents.Set([]agents.Agent{
		{Namespace: "homepage", Name: "reader", Tools: []string{"read_file"}},
		{Namespace: "homepage", Name: "marketing", Tools: []string{"list_repos"},
			Skills: []agents.Skill{{Name: "thumbnail", Description: "Make a thumbnail.", Dir: "agents/marketing/skills/thumbnail"}}},
	})
	names := func(path string) []string {
		code, out := rpcAt(t, m, path, "tok-alice", "tools/list", nil)
		if code != http.StatusOK {
			t.Fatalf("%s: %d", path, code)
		}
		var n []string
		for _, x := range out["result"].(map[string]any)["tools"].([]any) {
			n = append(n, x.(map[string]any)["name"].(string))
		}
		return n
	}
	if got := names("/mcp/homepage/reader"); len(got) != 1 || got[0] != "read_file" {
		t.Errorf("reader's tools = %v", got)
	}
	if got := names("/mcp/homepage/marketing"); strings.Join(got, ",") != "list_repos,load_skill,read_skill_file" {
		t.Errorf("marketing's tools = %v", got)
	}
	if got := names("/mcp"); len(got) != len(ToolNames())-1 {
		t.Errorf("/mcp tools = %v", got)
	}
	// A tool the agent lacks is refused before anything happens.
	_, out := rpcAt(t, m, "/mcp/homepage/reader", "tok-alice", "tools/call", map[string]any{"name": "propose_change", "arguments": map[string]any{
		"namespace": "garrison", "repo": "grafana", "path": "compose.yaml", "content": "x", "title": "t"}})
	if out["error"] == nil || fg.prs != 0 {
		t.Errorf("reader proposed a change: %v, %d pull requests", out, fg.prs)
	}
	// Open WebUI names the agent in a header, on the one /mcp connection.
	viaHeader := func(agent string) (int, []string) {
		body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list"})
		r := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(string(body)))
		r.Header.Set("Authorization", "Bearer tok-alice")
		r.Header.Set(AgentHeader, agent)
		w := httptest.NewRecorder()
		m.ServeHTTP(w, r)
		var out map[string]any
		json.Unmarshal(w.Body.Bytes(), &out)
		var n []string
		if res, ok := out["result"].(map[string]any); ok {
			for _, x := range res["tools"].([]any) {
				n = append(n, x.(map[string]any)["name"].(string))
			}
		}
		return w.Code, n
	}
	if code, got := viaHeader("homepage/reader"); code != http.StatusOK || len(got) != 1 || got[0] != "read_file" {
		t.Errorf("reader via header: %d %v", code, got)
	}
	// An agent nobody defined, or a model without an agent, gets nothing.
	for _, id := range []string{"homepage/nobody", ""} {
		if code, _ := viaHeader(id); code != http.StatusNotFound {
			t.Errorf("agent %q: %d", id, code)
		}
	}
	r := httptest.NewRequest(http.MethodPost, "/mcp/homepage/nobody", strings.NewReader("{}"))
	r.Header.Set("Authorization", "Bearer tok-alice")
	w := httptest.NewRecorder()
	m.ServeHTTP(w, r)
	if w.Code != http.StatusNotFound {
		t.Errorf("unknown agent in the path: %d", w.Code)
	}
	// carol is not in homepage: she may use its agents on public repos, not
	// those on private ones or kept to its people.
	m.Agents.Set([]agents.Agent{
		{Namespace: "homepage", Name: "desk", Tools: []string{"read_file"}, Repos: []string{"site"}},
		{Namespace: "homepage", Name: "keeper", Tools: []string{"read_file"}, Repos: []string{"secret"}},
		{Namespace: "homepage", Name: "inner", Tools: []string{"read_file"}, Visible: agents.VisibleMembers},
	})
	for agent, want := range map[string]int{"desk": http.StatusOK, "keeper": http.StatusForbidden, "inner": http.StatusForbidden} {
		if code, _ := rpcAt(t, m, "/mcp/homepage/"+agent, "tok-carol", "tools/list", nil); code != want {
			t.Errorf("carol and homepage/%s: %d, want %d", agent, code, want)
		}
		if code, _ := rpcAt(t, m, "/mcp/homepage/"+agent, "tok-bob", "tools/list", nil); code != http.StatusOK {
			t.Errorf("bob, a member, and homepage/%s: %d", agent, code)
		}
	}
	m.Agents.Set([]agents.Agent{
		{Namespace: "homepage", Name: "marketing", Tools: []string{"list_repos"},
			Skills: []agents.Skill{{Name: "thumbnail", Description: "Make a thumbnail.", Dir: "agents/marketing/skills/thumbnail"}}},
	})
	// Skills: load one, read its file, nothing outside it.
	call := func(tool string, args map[string]any) (string, bool) {
		_, out := rpcAt(t, m, "/mcp/homepage/marketing", "tok-bob", "tools/call", map[string]any{"name": tool, "arguments": args})
		res := out["result"].(map[string]any)
		isErr, _ := res["isError"].(bool)
		return res["content"].([]any)[0].(map[string]any)["text"].(string), isErr
	}
	if text, isErr := call("load_skill", map[string]any{"name": "thumbnail"}); isErr || !strings.Contains(text, "Use frame.svg.") || !strings.Contains(text, "- frame.svg") || strings.Contains(text, "- SKILL.md") {
		t.Errorf("load_skill = %q %v", text, isErr)
	}
	if text, isErr := call("read_skill_file", map[string]any{"skill": "thumbnail", "path": "frame.svg"}); isErr || text != "<svg/>" {
		t.Errorf("read_skill_file = %q %v", text, isErr)
	}
	if _, isErr := call("read_skill_file", map[string]any{"skill": "thumbnail", "path": "../../reader/prompt.md"}); !isErr {
		t.Error("read_skill_file left its skill")
	}
	if _, isErr := call("load_skill", map[string]any{"name": "nope"}); !isErr {
		t.Error("load_skill loaded a skill the agent does not have")
	}
}

func TestCatalogReadsUnknownAgentsFromGitea(t *testing.T) {
	fg := &fakeGitea{}
	gsrv := httptest.NewServer(fg)
	defer gsrv.Close()
	c := &Catalog{Gitea: &Gitea{URL: gsrv.URL, User: "admin", Password: "pw"}}
	if a, ok := c.Lookup(context.Background(), "homepage/marketing"); !ok || a.Description != "Media" {
		t.Errorf("homepage/marketing before any sync: %+v %v", a, ok)
	}
	if _, ok := c.Lookup(context.Background(), "garrison/operator"); !ok {
		t.Error("garrison/operator before any sync")
	}
	for _, id := range []string{"homepage/broken", "homepage/nobody", "../x", "nobody"} {
		if _, ok := c.Lookup(context.Background(), id); ok {
			t.Errorf("%s found", id)
		}
	}
}

func TestTheBrowserStaysAtHome(t *testing.T) {
	b := &Browser{Domain: "example.com"}
	for u, want := range map[string]bool{
		"https://todo.example.com/":         true,
		"https://example.com/x":             true,
		"https://a.b.example.com:443/p?q=1": true,
		"http://todo.example.com/":          false, // not https
		"https://example.com.evil.org/":     false,
		"https://evilexample.com/":          false,
		"https://gitea_gitea:3000/":         false,
		"https://10.0.0.1/":                 false,
		"https://user:pw@todo.example.com/": false,
		"file:///etc/passwd":                false,
		"javascript:alert(1)":               false,
		"https://TODO.EXAMPLE.COM/":         true,
	} {
		if got := b.Allowed(u); got != want {
			t.Errorf("Allowed(%q) = %v, want %v", u, got, want)
		}
	}
}

func TestScreenshotChecksBeforeShooting(t *testing.T) {
	m, fg := setup(t)
	// No browser set up: refused, nothing proposed.
	if text, isErr := callText(t, m, "tok-alice", "screenshot", map[string]any{"namespace": "garrison", "repo": "grafana", "path": "a.png", "url": "https://grafana.example.com/", "title": "t"}); !isErr || !strings.Contains(text, "no browser") {
		t.Errorf("without a browser: %q %v", text, isErr)
	}
	m.Tools.Browser = &Browser{URL: "http://127.0.0.1:1", Domain: "example.com"}
	// bob may not write to grafana: refused before the browser runs (which
	// here would fail differently).
	if text, isErr := callText(t, m, "tok-bob", "screenshot", map[string]any{"namespace": "garrison", "repo": "grafana", "path": "a.png", "url": "https://grafana.example.com/", "title": "t"}); !isErr || !strings.Contains(text, "not allowed") {
		t.Errorf("bob: %q %v", text, isErr)
	}
	for name, args := range map[string]map[string]any{
		"not a png":     {"namespace": "garrison", "repo": "grafana", "path": "a.jpg", "url": "https://grafana.example.com/", "title": "t"},
		"too wide":      {"namespace": "garrison", "repo": "grafana", "path": "a.png", "url": "https://grafana.example.com/", "title": "t", "width": 9000},
		"another site":  {"namespace": "garrison", "repo": "grafana", "path": "a.png", "url": "https://example.org/", "title": "t"},
		"internal host": {"namespace": "garrison", "repo": "grafana", "path": "a.png", "url": "http://gitea_gitea:3000/", "title": "t"},
	} {
		if text, isErr := callText(t, m, "tok-alice", "screenshot", args); !isErr {
			t.Errorf("%s: accepted: %q", name, text)
		}
	}
	if fg.prs != 0 {
		t.Errorf("%d pull requests opened", fg.prs)
	}
}

func TestTheTokenIsNeverShown(t *testing.T) {
	b, _ := json.Marshal(Person{Name: "bob", Token: "secret-token"})
	if strings.Contains(string(b), "secret-token") {
		t.Errorf("person as JSON: %s", b)
	}
}

// A patch well below MaxPatch gets through the MCP endpoint: its request
// limit leaves room for the largest patch.
func TestALargePatchGetsThrough(t *testing.T) {
	m, fg := setup(t)
	patch := "diff --git a/x b/x\n" + strings.Repeat("+a line of the file\n", 100000) // about 2 MB
	code, out := rpc(t, m, "tok-alice", "tools/call", map[string]any{"name": "propose_patch",
		"arguments": map[string]any{"namespace": "garrison", "repo": "grafana", "patch": patch, "title": "Add x"}})
	if code != http.StatusOK || out["error"] != nil || len(fg.patches) != 1 {
		t.Fatalf("%d %v, %d patches reached Gitea", code, out["error"], len(fg.patches))
	}
	// Beyond the limit the tool says so itself.
	patch = strings.Repeat("+", MaxPatch+1)
	text, isErr := callText(t, m, "tok-alice", "propose_patch", map[string]any{"namespace": "garrison", "repo": "grafana", "patch": patch, "title": "Add x"})
	if !isErr || !strings.Contains(text, "larger than 8 MB") {
		t.Errorf("a patch beyond the limit: %.200q", text)
	}
}

// Every namespace is listed, not only Gitea's first page of them.
func TestNamespacesBeyondTheFirstPage(t *testing.T) {
	gitea := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/orgs" {
			http.NotFound(w, r)
			return
		}
		var page []map[string]string
		switch r.URL.Query().Get("page") {
		case "1", "":
			for i := range 50 {
				page = append(page, map[string]string{"username": fmt.Sprintf("ns-%02d", i)})
			}
		case "2":
			page = append(page, map[string]string{"username": "photos"})
		}
		json.NewEncoder(w).Encode(page)
	}))
	defer gitea.Close()
	g := &Gitea{URL: gitea.URL, User: "admin", Password: "pw"}
	nss, err := g.Namespaces(context.Background(), "")
	if err != nil || len(nss) != 51 || nss[50].Name != "photos" {
		t.Fatalf("%d namespaces, %v", len(nss), err)
	}
}

// The person's token goes only where the sign-in in front takes it away
// again: the dashboard and private environments. Any other app would get
// it as it is.
func TestTheTokenGoesOnlyToPrivatePages(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "applied.json"), []byte(`{"apps":{
		"homepage/todo@main":{"namespace":"homepage","stack":"todo","private":true,"hosts":["todo","todo-api"]},
		"homepage/blog@main":{"namespace":"homepage","stack":"blog","hosts":["blog"]}}}`), 0o644)
	tools := &Tools{WorkDir: dir}
	b := &Browser{Domain: "example.com", Private: tools.PrivateHost}
	for u, want := range map[string]bool{
		"https://example.com/":              true, // the dashboard
		"https://todo.example.com/x":        true,
		"https://todo-api.example.com/":     true,
		"https://blog.example.com/":         false, // public: the app would see the token
		"https://unknown.example.com/":      false,
		"https://git.example.com/":          false,
		"https://x.todo.example.com/":       false,
		"https://todo.example.com.evil.org": false,
		"http://todo.example.com/":          false,
	} {
		if got := b.SendsToken(u); got != want {
			t.Errorf("SendsToken(%q) = %v, want %v", u, got, want)
		}
	}
	if (&Browser{Domain: "example.com"}).SendsToken("https://todo.example.com/") {
		t.Error("the token went out without knowing what is private")
	}
}

func TestTheHowToNamesTheCommander(t *testing.T) {
	o := &OpenWebUI{}
	list := []agents.Agent{{Namespace: "todo", Name: "operator", Description: "Runs todo"}}
	for _, l := range [][]agents.Agent{list, nil} {
		if got := o.howTo(Namespace{Name: "todo"}, l, "commander"); !strings.Contains(got, "Not sure which agent fits? Ask `@commander`") {
			t.Errorf("how-to with a commander:\n%s", got)
		}
	}
	if got := o.howTo(Namespace{Name: "todo"}, list, ""); strings.Contains(got, "commander") {
		t.Errorf("how-to without one:\n%s", got)
	}
}
