package gateway

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMakingUnitsAndRepos(t *testing.T) {
	var made []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		as := r.Header.Get("Sudo")
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		switch r.URL.Path {
		case "/api/v1/orgs": // Gitea lets only its admins make organisations
			if as != "alice" {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			if body["username"] == "homepage" {
				w.WriteHeader(http.StatusUnprocessableEntity)
				return
			}
			made = append(made, "org "+body["username"].(string)+" "+body["visibility"].(string))
			io.WriteString(w, "{}")
		case "/api/v1/orgs/photos/repos":
			if as != "alice" {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			made = append(made, "repo photos/"+body["name"].(string))
			io.WriteString(w, `{"html_url":"https://git/photos/`+body["name"].(string)+`"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	var readied []string
	tools := &Tools{Gitea: &Gitea{URL: srv.URL, User: "admin", Password: "pw"},
		Ready: func(_ context.Context, ns string) (string, error) {
			readied = append(readied, ns)
			return "https://chat.home.test/channels/c-" + ns, nil
		},
		UnitChannel: func(_ context.Context, ns string) (string, error) {
			return "https://chat.home.test/channels/c-" + ns, nil
		}}
	call := func(who, tool string, args map[string]any) (string, error) {
		raw, _ := json.Marshal(args)
		return tools.Call(context.Background(), Person{Name: who}, nil, tool, raw)
	}
	// The agent answers once the unit is set up, not before.
	if out, err := call("alice", "create_unit", map[string]any{"name": "photos", "description": "Our photos"}); err != nil || !strings.Contains(out, "alice as its first admin") ||
		!strings.Contains(out, "It is ready") || !strings.Contains(out, "the agents (@operator and the others) work in photos: https://chat.home.test/channels/c-photos") || len(readied) != 1 {
		t.Errorf("alice: %q %v %v", out, err, readied)
	}
	if _, err := call("bob", "create_unit", map[string]any{"name": "bobs"}); err == nil || !strings.Contains(err.Error(), "only root admins may") {
		t.Errorf("bob: %v", err)
	}
	if _, err := call("alice", "create_unit", map[string]any{"name": "homepage"}); err == nil || !strings.Contains(err.Error(), "already") ||
		!strings.Contains(err.Error(), "its channel is https://chat.home.test/channels/c-homepage") {
		t.Errorf("an existing one: %v", err)
	}
	for _, bad := range []string{"Photos", "a.b", "../x"} {
		if _, err := call("alice", "create_unit", map[string]any{"name": bad}); err == nil {
			t.Errorf("unit %q made", bad)
		}
	}
	if out, err := call("alice", "create_repo", map[string]any{"unit": "photos", "repo": "immich"}); err != nil || !strings.Contains(out, "https://git/photos/immich") {
		t.Errorf("repo: %q %v", out, err)
	}
	if _, err := call("bob", "create_repo", map[string]any{"unit": "photos", "repo": "mine"}); err == nil || !strings.Contains(err.Error(), "may not make repos in photos") {
		t.Errorf("bob's repo: %v", err)
	}
	for _, bad := range []string{"config", "Immich", "a_b"} {
		if _, err := call("alice", "create_repo", map[string]any{"unit": "photos", "repo": bad}); err == nil {
			t.Errorf("repo %q made", bad)
		}
	}
	if strings.Join(made, ",") != "org photos limited,repo photos/immich" {
		t.Errorf("made %v", made)
	}
	// Not without a person.
	raw, _ := json.Marshal(map[string]any{"name": "x"})
	if _, err := tools.Call(context.Background(), Person{Name: "agent-x", Unattended: true, Owner: "alice"}, nil, "create_unit", raw); err == nil {
		t.Error("a routine made a unit")
	}
}

func TestTheApprovalCheckReadsStacks(t *testing.T) {
	files := map[string]string{
		"todo.yaml":   "source: todo\nentrypoint: web\nservices:\n  web: { image: a, port: 80 }\nenvironments:\n  main: {}\n",
		"ghost.yaml":  "source: ghost\nentrypoint: web\nservices:\n  web: { image: a, port: 80 }\nenvironments:\n  main: {}\n",
		"broken.yaml": "entrypoint: web\n",
		"named.yaml":  "stack: other\nentrypoint: web\nservices:\n  web: { image: a, port: 80 }\nenvironments:\n  main: {}\n",
	}
	restores := map[string]string{
		"good.yaml":    "stack: todo\nsnapshot: deploy-20261113-150405-abc1234\n",
		"nostack.yaml": "stack: broken\nsnapshot: s1\n",
		"noenv.yaml":   "stack: todo\nenvironment: dev\nsnapshot: s1\n",
		"bad.yaml":     "stack: todo\n",
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/api/v1")
		switch {
		case strings.HasSuffix(p, "/config/contents/stacks"):
			var list []map[string]string
			for n := range files {
				list = append(list, map[string]string{"name": n, "type": "file"})
			}
			list = append(list, map[string]string{"name": "notes.txt", "type": "file"})
			json.NewEncoder(w).Encode(list)
		case strings.Contains(p, "/config/raw/stacks/"):
			n := p[strings.LastIndex(p, "/")+1:]
			io.WriteString(w, files[n])
		case strings.HasSuffix(p, "/config/contents/restores"):
			var list []map[string]string
			for n := range restores {
				list = append(list, map[string]string{"name": n, "type": "file"})
			}
			json.NewEncoder(w).Encode(list)
		case strings.Contains(p, "/config/raw/restores/"):
			io.WriteString(w, restores[p[strings.LastIndex(p, "/")+1:]])
		case p == "/repos/homepage/todo":
			io.WriteString(w, "{}")
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	g := &Gitea{URL: srv.URL, User: "admin", Password: "pw"}
	problems, err := g.stackProblems(context.Background(), "homepage", "abc")
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(problems, "\n")
	for _, want := range []string{"stacks/ghost.yaml: there is no repo homepage/ghost", "stacks/broken.yaml:", "stacks/named.yaml: stack: other", "stacks/notes.txt: stacks/ holds only",
		"restores/nostack.yaml: there is no valid stack broken", "restores/noenv.yaml: todo has no environment dev", "restores/bad.yaml: snapshot must be"} {
		if !strings.Contains(got, want) {
			t.Errorf("problems lack %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "todo.yaml") || strings.Contains(got, "good.yaml") {
		t.Errorf("a good stack has problems:\n%s", got)
	}
	if problems, _ := g.stackProblems(context.Background(), "garrison", "abc"); len(problems) == 0 || !strings.Contains(problems[0], "garrison holds the platform") {
		t.Errorf("garrison: %v", problems)
	}
}

// A repo from a template starts with a working app, written as the person,
// its README naming the stack to propose.
func TestARepoFromATemplate(t *testing.T) {
	var ops []map[string]string
	var by string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/orgs/lab/repos":
			io.WriteString(w, `{"html_url":"https://git/lab/example"}`)
		case "/api/v1/repos/lab/example/contents/README.md": // auto_init made it
			io.WriteString(w, `{"sha":"r1"}`)
		case "/api/v1/repos/lab/example/contents":
			by = r.Header.Get("Sudo")
			var body struct {
				Branch string              `json:"branch"`
				Files  []map[string]string `json:"files"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			ops = body.Files
			w.WriteHeader(http.StatusCreated)
			io.WriteString(w, "{}")
		case "/api/v1/repos/lab/example/raw/README.md":
			for _, op := range ops {
				if op["path"] == "README.md" {
					data, _ := base64.StdEncoding.DecodeString(op["content"])
					w.Write(data)
					return
				}
			}
			io.WriteString(w, "# example\n")
		case "/api/v1/repos/lab/example":
			io.WriteString(w, `{"permissions":{"pull":true}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	tools := &Tools{Gitea: &Gitea{URL: srv.URL, User: "admin", Password: "pw"}}
	raw, _ := json.Marshal(map[string]any{"unit": "lab", "repo": "example", "template": "todo"})
	out, err := tools.Call(context.Background(), Person{Name: "alice"}, nil, "create_repo", raw)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, op := range ops {
		data, _ := base64.StdEncoding.DecodeString(op["content"])
		got[op["path"]+" "+op["operation"]] = string(data)
	}
	if by != "alice" || len(got) != 9 || !strings.Contains(got["page.html create"], "<h1>Todo</h1>") ||
		!strings.Contains(got["go.mod create"], "module todo") || got["main.go.tmpl create"] != "" ||
		!strings.Contains(got[".gitea/workflows/pipeline.yaml create"], "buildctl") || !strings.Contains(got["Dockerfile create"], "EXPOSE 8080") ||
		!strings.Contains(got["README.md update"], `image: "{{registry}}/lab/example:{{commit}}"`) {
		t.Errorf("by %s: %v", by, keys(got))
	}
	if !strings.Contains(out, "from the template todo") || !strings.Contains(out, "source: example") {
		t.Errorf("out: %s", out)
	}
	raw, _ = json.Marshal(map[string]any{"unit": "lab", "repo": "other", "template": "../unit-config"})
	if _, err := tools.Call(context.Background(), Person{Name: "alice"}, nil, "create_repo", raw); err == nil {
		t.Error("a template outside app/")
	}
}

func keys(m map[string]string) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestAConfigFileTheDeployerWouldRefuseIsNotProposed(t *testing.T) {
	valid := "entrypoint: web\nservices:\n  web:\n    image: traefik/whoami:v1.11\n    port: 80\nenvironments:\n  main: {}\n"
	for _, c := range []struct{ ns, repo, path, content, want string }{
		{"photos", "config", "stacks/gallery.yaml", valid, ""},
		{"photos", "config", "config/stacks/gallery.yaml", valid, "propose stacks/gallery.yaml, not config/stacks/gallery.yaml"},
		{"photos", "config", "stacks/gallery.yaml", "services:\n  web:\n    image: traefik/whoami:v1.11\n", "the deployer would refuse stacks/gallery.yaml"},
		{"photos", "config", "stacks/gallery", valid, "holds only <name>.yaml files"},
		{"photos", "config", "README.md", "anything", ""},
		{"photos", "gallery", "config/stacks/x.yaml", "anything", ""}, // an app's repo: its own business
	} {
		err := checkConfigFile(c.ns, c.repo, c.path, c.content)
		if c.want == "" && err != nil || c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)) {
			t.Errorf("%s/%s %s: %v, want %q", c.ns, c.repo, c.path, err, c.want)
		}
	}
}
