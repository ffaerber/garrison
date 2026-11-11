package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// fakeGitea answers like Gitea: alice may write to system, bob may only read.
type fakeGitea struct {
	mu   sync.Mutex
	prs  int
	sudo []string
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
		io.WriteString(w, `[{"username":"system","description":"the platform"},{"username":"homepage"}]`)
	case p == "/orgs/system/repos":
		json.NewEncoder(w).Encode([]Repo{{Name: "grafana"}})
	case p == "/repos/system/grafana/raw/compose.yaml":
		io.WriteString(w, "services: {}\n")
	case p == "/repos/system/grafana/contents/compose.yaml" && r.Method == http.MethodGet:
		io.WriteString(w, `{"sha":"abc"}`)
	case p == "/repos/system/grafana/contents/compose.yaml":
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if body["branch"] != "main" || body["new_branch"] == "" {
			http.Error(w, `{"message":"must write to a new branch"}`, http.StatusBadRequest)
			return
		}
		io.WriteString(w, "{}")
	case p == "/repos/system/grafana/pulls" && r.Method == http.MethodPost:
		f.prs++
		io.WriteString(w, `{"number":7,"html_url":"https://git.example.com/system/grafana/pulls/7"}`)
	case strings.HasSuffix(p, "/pulls"):
		io.WriteString(w, "[]")
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
	os.WriteFile(filepath.Join(dir, "applied.json"), []byte(`{"stacks":{"grafana":"0123456789abcdef"}}`), 0o644)
	run := func(_ context.Context, _ []string, _ []byte, name string, args ...string) ([]byte, error) {
		if name == "docker" && len(args) > 1 && args[0] == "service" && args[1] == "ls" {
			return []byte("grafana_grafana\t1/1\tgrafana/grafana:12\nhello_web\t1/1\twhoami\n"), nil
		}
		return []byte("log line\n"), nil
	}
	tools := &Tools{Gitea: &Gitea{URL: gsrv.URL, User: "admin", Password: "pw"}, Run: run, WorkDir: dir, Prometheus: prom.URL}
	return &MCP{Tools: tools, Verifier: fakeVerifier{"tok-alice": {Name: "alice", Admin: true}, "tok-bob": {Name: "bob"}}}, fg
}

// rpc posts one JSON-RPC request with a bearer token and returns status and decoded body.
func rpc(t *testing.T, m *MCP, token, method string, params any) (int, map[string]any) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	r := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(string(body)))
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
	if isErr || !strings.Contains(text, "system") || !strings.Contains(text, "homepage") {
		t.Errorf("list_namespaces = %q", text)
	}
	text, isErr = callText(t, m, "tok-bob", "list_stacks", map[string]any{"namespace": "system"})
	if isErr || !strings.Contains(text, "grafana (commit 0123456789ab)") || strings.Contains(text, "hello") {
		t.Errorf("list_stacks = %q", text)
	}
	if text, isErr = callText(t, m, "tok-bob", "service_logs", map[string]any{"namespace": "system", "service": "grafana_grafana"}); isErr || text != "log line\n" {
		t.Errorf("service_logs = %q", text)
	}
	if text, isErr = callText(t, m, "tok-bob", "service_usage", map[string]any{"namespace": "system", "stack": "grafana"}); isErr || !strings.Contains(text, "12.5") {
		t.Errorf("service_usage = %q", text)
	}
	if text, isErr = callText(t, m, "tok-bob", "query_metrics", map[string]any{"query": "up"}); isErr || !strings.Contains(text, "grafana_grafana") {
		t.Errorf("query_metrics = %q", text)
	}
	if text, isErr = callText(t, m, "tok-bob", "read_file", map[string]any{"namespace": "system", "repo": "grafana", "path": "compose.yaml"}); isErr || text != "services: {}\n" {
		t.Errorf("read_file = %q", text)
	}
}

func TestToolsStayInTheirNamespace(t *testing.T) {
	m, _ := setup(t)
	for _, c := range []struct {
		tool string
		args map[string]any
	}{
		{"service_logs", map[string]any{"namespace": "system", "service": "hello_web"}},            // not a system stack
		{"service_logs", map[string]any{"namespace": "system", "service": "grafana_grafana; id"}},  // not a name
		{"stack_status", map[string]any{"namespace": "homepage", "stack": "grafana"}},              // wrong namespace
		{"read_file", map[string]any{"namespace": "system", "repo": "grafana", "path": "../../x"}}, // outside the repo
		{"read_file", map[string]any{"namespace": "../admin", "repo": "grafana", "path": "a"}},     // not a namespace
		{"query_metrics", map[string]any{"query": "bad("}},                                         // Prometheus refuses
	} {
		if text, isErr := callText(t, m, "tok-alice", c.tool, c.args); !isErr {
			t.Errorf("%s %v accepted: %q", c.tool, c.args, text)
		}
	}
}

func TestProposalsActAsThePerson(t *testing.T) {
	args := map[string]any{"namespace": "system", "repo": "grafana", "path": "compose.yaml", "content": "services: {x: {}}\n", "title": "Add x"}
	m, fg := setup(t)
	text, isErr := callText(t, m, "tok-bob", "propose_change", args)
	if !isErr || !strings.Contains(text, "not allowed: bob") || fg.prs != 0 {
		t.Errorf("bob: %q, prs %d", text, fg.prs)
	}
	text, isErr = callText(t, m, "tok-alice", "propose_change", args)
	if isErr || fg.prs != 1 || !strings.Contains(text, "https://git.example.com/system/grafana/pulls/7") || !strings.Contains(text, "Nothing is changed yet") {
		t.Errorf("alice: %q, prs %d", text, fg.prs)
	}
	for _, s := range fg.sudo {
		if s != "alice" && s != "bob" {
			t.Errorf("Gitea called as %q, not as the person", s)
		}
	}
}

func TestOpenWebUISync(t *testing.T) {
	fg := &fakeGitea{}
	gsrv := httptest.NewServer(fg)
	defer gsrv.Close()
	var mu sync.Mutex
	models := map[string]owuiModel{"agent-gone": {ID: "agent-gone", Name: "agent-gone"}, "ns-system": {ID: "ns-system", Name: "system"}, "other": {ID: "other"}}
	channels := map[string]owuiChannel{
		"c-gone":  {ID: "c-gone", Name: "gone", Meta: map[string]any{ChannelMarker: "gone"}},
		"c-other": {ID: "c-other", Name: "random"},
	}
	nextChannel := 0
	profileName := "Installer"
	messages := map[string][]owuiMessage{} // channel id -> messages
	var calls []string
	ow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, r.URL.Path)
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
		case "/api/v1/models/create", "/api/v1/models/model/update":
			var m owuiModel
			json.NewDecoder(r.Body).Decode(&m)
			models[m.ID] = m
			io.WriteString(w, "{}")
		case "/api/v1/models/model/delete":
			var b map[string]string
			json.NewDecoder(r.Body).Decode(&b)
			delete(models, b["id"])
			io.WriteString(w, "true")
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
	o := &OpenWebUI{URL: ow.URL, Email: "e", Password: "p", BaseModel: "gpt-oss:120b", ToolServerID: "homecloud", Domain: "example.com",
		Gitea: &Gitea{URL: gsrv.URL, User: "admin", Password: "pw"}}
	if err := o.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	sys, ok := models["agent-system"]
	if !ok || sys.Name != "agent-system" || models["agent-homepage"].Name != "agent-homepage" {
		t.Fatalf("models = %v", models)
	}
	if _, ok := models["agent-gone"]; ok {
		t.Error("the agent of a deleted namespace was kept")
	}
	if _, ok := models["ns-system"]; ok {
		t.Error("an agent of an earlier version was kept")
	}
	if _, ok := models["other"]; !ok {
		t.Error("a model the sync does not own was removed")
	}
	if b, ok := models["gpt-oss:120b"]; !ok || b.Meta["hidden"] != true || len(b.AccessGrants) != 1 {
		t.Errorf("base model entry = %+v", b)
	}
	if sys.BaseModelID != "gpt-oss:120b" || !strings.Contains(sys.Params["system"].(string), `namespace "system"`) {
		t.Errorf("agent-system = %+v", sys)
	}
	if ids := sys.Meta["toolIds"].([]any); len(ids) != 1 || ids[0] != "server:mcp:homecloud" {
		t.Errorf("toolIds = %v", ids)
	}
	// One channel per namespace, everyone may post, the gone one removed, others kept.
	byNS := map[string]owuiChannel{}
	for _, c := range channels {
		if ns, ok := c.Meta[ChannelMarker].(string); ok {
			byNS[ns] = c
		}
	}
	if len(byNS) != 2 || byNS["system"].Name != "system" || byNS["homepage"].Name != "homepage" {
		t.Errorf("channels = %v", channels)
	}
	if _, ok := channels["c-gone"]; ok {
		t.Error("the channel of a deleted namespace was kept")
	}
	if _, ok := channels["c-other"]; !ok {
		t.Error("a channel the sync does not own was removed")
	}
	// A renamed channel is put back.
	renamed := byNS["system"]
	renamed.Name = "renamed-by-an-admin"
	channels[renamed.ID] = renamed
	o.Sync(context.Background())
	if channels[renamed.ID].Name != "system" {
		t.Errorf("renamed channel not put back: %v", channels[renamed.ID])
	}
	// The control plane's account says it is not the agent.
	if profileName != AccountName {
		t.Errorf("account name = %q", profileName)
	}
	// Each namespace channel has the how-to pinned, once.
	sysID := byNS["system"].ID
	pinnedHowTos := func() int {
		n := 0
		for _, m := range messages[sysID] {
			if m.IsPinned && strings.HasPrefix(m.Content, HowToMarker) && strings.Contains(m.Content, "`@agent-system") {
				n++
			}
		}
		return n
	}
	if pinnedHowTos() != 1 {
		t.Errorf("system channel messages = %+v", messages[sysID])
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
		if strings.Contains(c, "create") || strings.Contains(c, "update") || strings.Contains(c, "delete") || strings.HasSuffix(c, "/post") || strings.HasSuffix(c, "/pin") {
			t.Errorf("second sync called %s", c)
		}
	}
}
