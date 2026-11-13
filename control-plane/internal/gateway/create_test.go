package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMakingNamespacesAndRepos(t *testing.T) {
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
	tools := &Tools{Gitea: &Gitea{URL: srv.URL, User: "admin", Password: "pw"}}
	call := func(who, tool string, args map[string]any) (string, error) {
		raw, _ := json.Marshal(args)
		return tools.Call(context.Background(), Person{Name: who}, nil, tool, raw)
	}
	if out, err := call("alice", "create_namespace", map[string]any{"name": "photos", "description": "Our photos"}); err != nil || !strings.Contains(out, "alice as its first admin") {
		t.Errorf("alice: %q %v", out, err)
	}
	if _, err := call("bob", "create_namespace", map[string]any{"name": "bobs"}); err == nil || !strings.Contains(err.Error(), "only root admins may") {
		t.Errorf("bob: %v", err)
	}
	if _, err := call("alice", "create_namespace", map[string]any{"name": "homepage"}); err == nil || !strings.Contains(err.Error(), "already") {
		t.Errorf("an existing one: %v", err)
	}
	for _, bad := range []string{"Photos", "a.b", "../x"} {
		if _, err := call("alice", "create_namespace", map[string]any{"name": bad}); err == nil {
			t.Errorf("namespace %q made", bad)
		}
	}
	if out, err := call("alice", "create_repo", map[string]any{"namespace": "photos", "repo": "immich"}); err != nil || !strings.Contains(out, "https://git/photos/immich") {
		t.Errorf("repo: %q %v", out, err)
	}
	if _, err := call("bob", "create_repo", map[string]any{"namespace": "photos", "repo": "mine"}); err == nil || !strings.Contains(err.Error(), "may not make repos in photos") {
		t.Errorf("bob's repo: %v", err)
	}
	for _, bad := range []string{"config", "Immich", "a_b"} {
		if _, err := call("alice", "create_repo", map[string]any{"namespace": "photos", "repo": bad}); err == nil {
			t.Errorf("repo %q made", bad)
		}
	}
	if strings.Join(made, ",") != "org photos limited,repo photos/immich" {
		t.Errorf("made %v", made)
	}
	// Not without a person.
	raw, _ := json.Marshal(map[string]any{"name": "x"})
	if _, err := tools.Call(context.Background(), Person{Name: "agent-x", Unattended: true, Owner: "alice"}, nil, "create_namespace", raw); err == nil {
		t.Error("a routine made a namespace")
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
	if problems, _ := g.stackProblems(context.Background(), "system", "abc"); len(problems) == 0 || !strings.Contains(problems[0], "system holds the platform") {
		t.Errorf("system: %v", problems)
	}
}
