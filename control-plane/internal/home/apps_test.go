package home

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"garrison/control-plane/internal/events"
	"garrison/control-plane/internal/health"
)

type fixedApps []health.App

func (f fixedApps) Apps(context.Context) ([]health.App, error) { return f, nil }

type brokenApps struct{}

func (brokenApps) Apps(context.Context) ([]health.App, error) { return nil, errors.New("no deployer") }

func appsServer() *Server {
	s, _ := server()
	s.Access = access{
		"root":  {All: true},
		"dave":  {Repos: map[string]bool{"todo/app": true, "todo/config": true}},
		"erin":  {Members: map[string]bool{"lab": true}},
		"frank": {},
	}
	s.Apps = fixedApps{
		{Unit: "garrison", App: "grafana", Environment: "main", Stack: "grafana", Hosts: []string{"grafana.home.test"}, Desired: 1, Running: 1},
		{Unit: "lab", App: "secret", Environment: "main", Stack: "secret", Hosts: []string{"secret.home.test"}, Desired: 1, Running: 1},
		{Unit: "todo", App: "todo", Environment: "dev", Stack: "todo", Hosts: []string{"dev-todo.home.test"}, Desired: 1, Running: 0},
		{Unit: "todo", App: "todo", Environment: "main", Stack: "todo", Hosts: []string{"todo.home.test"}, Desired: 2, Running: 1},
	}
	return s
}

func apiApps(t *testing.T, s *Server, who string) []appView {
	t.Helper()
	w := get(s, who, "/api/apps")
	if w.Code != 200 {
		t.Fatalf("%s: /api/apps: %d %s", who, w.Code, w.Body)
	}
	var got []appView
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	return got
}

func names(apps []appView) string {
	var n []string
	for _, a := range apps {
		n = append(n, a.Unit+"/"+a.App+"/"+a.Environment+"="+a.State)
	}
	return strings.Join(n, " ")
}

func TestAppsAsEachMaySee(t *testing.T) {
	s := appsServer()
	for who, want := range map[string]string{
		// A root admin sees every unit's.
		"root": "garrison/grafana/main=up lab/secret/main=up todo/todo/dev=down todo/todo/main=starting",
		// Someone who reads todo's config sees todo's, never a private unit's.
		"dave": "garrison/grafana/main=up todo/todo/dev=down todo/todo/main=starting",
		// A member of the private unit lab sees lab's.
		"erin": "garrison/grafana/main=up lab/secret/main=up",
		// Everyone sees the platform's own.
		"frank": "garrison/grafana/main=up",
	} {
		if got := names(apiApps(t, s, who)); got != want {
			t.Errorf("%s sees %q, want %q", who, got, want)
		}
	}
	if got := apiApps(t, s, "dave")[1].URLs; len(got) != 1 || got[0] != "https://dev-todo.home.test/" {
		t.Errorf("the dev environment's address: %v", got)
	}
}

func TestAppsPage(t *testing.T) {
	s := appsServer()
	w := get(s, "dave", "/apps")
	body := w.Body.String()
	if w.Code != 200 {
		t.Fatalf("/apps: %d %s", w.Code, body)
	}
	for _, want := range []string{`href="https://todo.home.test/"`, `href="https://grafana.home.test/"`, `data-app="todo/todo/dev"`, `<a href="/apps" class="on">Apps</a>`} {
		if !strings.Contains(body, want) {
			t.Errorf("the page lacks %s", want)
		}
	}
	if strings.Contains(body, "secret.home.test") {
		t.Error("dave sees a private unit's app")
	}
	if strings.Index(body, "<h2>garrison</h2>") > strings.Index(body, "<h2>todo</h2>") {
		t.Error("garrison's own should come first")
	}
	if w := get(s, "", "/api/apps"); w.Code != http.StatusUnauthorized {
		t.Errorf("/api/apps without a person: %d", w.Code)
	}
}

func TestAppsWithoutTheDeployer(t *testing.T) {
	s := appsServer()
	s.Apps = brokenApps{}
	if w := get(s, "dave", "/api/apps"); w.Code != http.StatusServiceUnavailable {
		t.Errorf("/api/apps: %d", w.Code)
	}
	if w := get(s, "dave", "/apps"); w.Code != 200 || !strings.Contains(w.Body.String(), "cannot be read right now") {
		t.Errorf("/apps: %d %s", w.Code, w.Body)
	}
	s.Apps = nil
	if w := get(s, "dave", "/apps"); w.Code != http.StatusNotFound {
		t.Errorf("/apps without Apps: %d", w.Code)
	}
}

func TestSeesApp(t *testing.T) {
	v := events.Visibility{Repos: map[string]bool{"todo/config": true}}
	if !SeesApp(v, "garrison") || !SeesApp(v, "todo") || SeesApp(v, "lab") {
		t.Error("SeesApp")
	}
}
