package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeBuilderGitea keeps users, teams, team members, org secrets and variables.
type fakeBuilderGitea struct {
	mu        sync.Mutex
	users     map[string]string // name -> password
	teams     map[string]map[string]any
	members   []string
	secrets   map[string]map[string]string // org -> name -> value
	variables map[string]string
	tokens    int
}

func (f *fakeBuilderGitea) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	u, p, _ := r.BasicAuth()
	p0 := strings.TrimPrefix(r.URL.Path, "/api/v1")
	var body map[string]any
	json.NewDecoder(r.Body).Decode(&body)
	// A build account may only create its own token.
	if strings.HasSuffix(p0, "/tokens") {
		owner := strings.Split(p0, "/")[2]
		if u != owner || f.users[u] != p {
			http.Error(w, "unauthorized", 401)
			return
		}
		scopes, _ := body["scopes"].([]any)
		if len(scopes) != 1 || scopes[0] != "write:package" {
			http.Error(w, "wrong scopes", 400)
			return
		}
		f.tokens++
		w.WriteHeader(201)
		io.WriteString(w, `{"sha1":"tok`+string(rune('0'+f.tokens))+`"}`)
		return
	}
	if u != "admin" || p != "pw" {
		http.Error(w, "unauthorized", 401)
		return
	}
	switch {
	case p0 == "/orgs":
		io.WriteString(w, `[{"username":"system"},{"username":"homepage"}]`)
	case strings.Contains(p0, "/actions/variables/"):
		parts := strings.Split(p0, "/")
		f.variables[parts[2]+"/"+parts[5]] = body["value"].(string)
		w.WriteHeader(204)
	case strings.HasSuffix(p0, "/actions/secrets") && r.Method == "GET":
		org := strings.Split(p0, "/")[2]
		var out []map[string]string
		for n := range f.secrets[org] {
			out = append(out, map[string]string{"name": n})
		}
		json.NewEncoder(w).Encode(out)
	case strings.Contains(p0, "/actions/secrets/"):
		parts := strings.Split(p0, "/")
		if f.secrets[parts[2]] == nil {
			f.secrets[parts[2]] = map[string]string{}
		}
		f.secrets[parts[2]][parts[5]] = body["data"].(string)
		w.WriteHeader(201)
	case strings.HasSuffix(p0, "/teams") && r.Method == "GET":
		var out []map[string]any
		for _, t := range f.teams {
			out = append(out, t)
		}
		json.NewEncoder(w).Encode(out)
	case strings.HasSuffix(p0, "/teams"):
		body["id"] = 7
		f.teams[body["name"].(string)] = body
		w.WriteHeader(201)
		json.NewEncoder(w).Encode(body)
	case strings.HasPrefix(p0, "/teams/7/members/"):
		f.members = append(f.members, strings.TrimPrefix(p0, "/teams/7/members/"))
		w.WriteHeader(204)
	case strings.HasPrefix(p0, "/users/") && r.Method == "GET":
		if _, ok := f.users[strings.TrimPrefix(p0, "/users/")]; !ok {
			http.Error(w, `{"message":"not found"}`, 404)
			return
		}
		io.WriteString(w, "{}")
	case p0 == "/admin/users":
		f.users[body["username"].(string)] = body["password"].(string)
		w.WriteHeader(201)
		io.WriteString(w, "{}")
	case strings.HasPrefix(p0, "/admin/users/"):
		f.users[strings.TrimPrefix(p0, "/admin/users/")] = body["password"].(string)
		io.WriteString(w, "{}")
	default:
		http.NotFound(w, r)
	}
}

func TestBuilders(t *testing.T) {
	f := &fakeBuilderGitea{users: map[string]string{}, teams: map[string]map[string]any{}, secrets: map[string]map[string]string{}, variables: map[string]string{}}
	srv := httptest.NewServer(f)
	defer srv.Close()
	b := &Builders{Gitea: &Gitea{URL: srv.URL, User: "admin", Password: "pw"}, Domain: "example.com", CA: "-----BEGIN CERTIFICATE-----"}
	if err := b.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.users["homepage-builder"]; !ok {
		t.Fatalf("users = %v", f.users)
	}
	if _, ok := f.users["system-builder"]; ok {
		t.Error("system got a build account; it builds on its own runner")
	}
	team := f.teams["builders"]
	if units, _ := team["units_map"].(map[string]any); units["repo.packages"] != "write" || len(units) != 1 || team["permission"] != "read" {
		t.Errorf("team = %v", team)
	}
	if len(f.members) != 1 || f.members[0] != "homepage-builder" {
		t.Errorf("members = %v", f.members)
	}
	if s := f.secrets["homepage"]; s["REGISTRY_USER"] != "homepage-builder" || s["REGISTRY_TOKEN"] != "tok1" {
		t.Errorf("secrets = %v", s)
	}
	if f.variables["homepage/REGISTRY"] != "git.example.com" || f.variables["homepage/REGISTRY_CA"] != "-----BEGIN CERTIFICATE-----" {
		t.Errorf("variables = %v", f.variables)
	}
	// Set up once: a second sync makes no new token.
	b.Sync(context.Background())
	if f.tokens != 1 {
		t.Errorf("tokens made: %d", f.tokens)
	}
	// Lost secret: the account gets a new password and token.
	delete(f.secrets["homepage"], "REGISTRY_TOKEN")
	b.Sync(context.Background())
	if f.secrets["homepage"]["REGISTRY_TOKEN"] != "tok2" {
		t.Errorf("secret not renewed: %v", f.secrets["homepage"])
	}
}
