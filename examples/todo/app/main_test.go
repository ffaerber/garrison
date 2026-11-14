package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func app() (*App, http.Handler) {
	a := &App{Store: &Memory{}}
	return a, a.Routes()
}

// as is the header the sign-in sets for who.
func as(who string) map[string]string { return map[string]string{userHeader: who} }

func do(h http.Handler, method, path, body, contentType string, header map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if contentType != "" {
		r.Header.Set("Content-Type", contentType)
	}
	for k, v := range header {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func form(v url.Values) string { return v.Encode() }

var ctx = context.Background()

func TestAddToggleDelete(t *testing.T) {
	a, h := app()
	if w := do(h, "POST", "/todos", form(url.Values{"title": {"  buy milk "}}), "application/x-www-form-urlencoded", as("alice")); w.Code != http.StatusSeeOther {
		t.Fatalf("add: %d", w.Code)
	}
	todos, _ := a.Store.List(ctx, "alice")
	if len(todos) != 1 || todos[0].Title != "buy milk" || todos[0].CreatedBy != "alice" {
		t.Fatalf("todos = %+v", todos)
	}
	w := do(h, "GET", "/", "", "", as("alice"))
	if !strings.Contains(w.Body.String(), "buy milk") || !strings.Contains(w.Body.String(), "alice's list · 1 open") {
		t.Errorf("page:\n%s", w.Body.String())
	}
	do(h, "POST", "/todos/1/toggle", "", "", as("alice"))
	if todos, _ := a.Store.List(ctx, "alice"); !todos[0].Done {
		t.Error("toggle did not mark it done")
	}
	do(h, "POST", "/todos/1/delete", "", "", as("alice"))
	if todos, _ := a.Store.List(ctx, "alice"); len(todos) != 0 {
		t.Error("delete did not remove it")
	}
}

func TestEveryoneHasTheirOwnList(t *testing.T) {
	a, h := app()
	do(h, "POST", "/todos", form(url.Values{"title": {"alice's"}}), "application/x-www-form-urlencoded", as("alice"))
	do(h, "POST", "/api/todos", `{"title":"bob's"}`, "application/json", as("bob"))
	for who, want := range map[string]string{"alice": "alice's", "bob": "bob's"} {
		var todos []Todo
		json.NewDecoder(do(h, "GET", "/api/todos", "", "", as(who)).Body).Decode(&todos)
		if len(todos) != 1 || todos[0].Title != want {
			t.Errorf("%s sees %+v", who, todos)
		}
		if page := do(h, "GET", "/", "", "", as(who)).Body.String(); strings.Count(page, "&#39;s</span>") != 1 {
			t.Errorf("%s's page:\n%s", who, page)
		}
	}
	// Bob cannot tick or delete alice's todo (id 1); it stays as it was.
	do(h, "POST", "/todos/1/toggle", "", "", as("bob"))
	do(h, "POST", "/todos/1/delete", "", "", as("bob"))
	if todos, _ := a.Store.List(ctx, "alice"); len(todos) != 1 || todos[0].Done {
		t.Errorf("bob changed alice's todo: %+v", todos)
	}
}

func TestOnlySignedInPeople(t *testing.T) {
	a, h := app()
	for _, c := range []struct{ method, path, body, ct string }{
		{"GET", "/", "", ""},
		{"GET", "/api/todos", "", ""},
		{"POST", "/api/todos", `{"title":"x"}`, "application/json"},
		{"POST", "/todos", form(url.Values{"title": {"x"}}), "application/x-www-form-urlencoded"},
	} {
		if w := do(h, c.method, c.path, c.body, c.ct, nil); w.Code != http.StatusUnauthorized {
			t.Errorf("%s %s without a person: %d", c.method, c.path, w.Code)
		}
	}
	if todos, _ := a.Store.List(ctx, ""); len(todos) != 0 {
		t.Errorf("stored for nobody: %+v", todos)
	}
}

func TestTitlesAreChecked(t *testing.T) {
	a, h := app()
	for _, title := range []string{"", "   ", strings.Repeat("x", 201)} {
		w := do(h, "POST", "/todos", form(url.Values{"title": {title}}), "application/x-www-form-urlencoded", as("alice"))
		if loc := w.Header().Get("Location"); !strings.Contains(loc, "error=") {
			t.Errorf("title %q accepted: %s", title, loc)
		}
	}
	if todos, _ := a.Store.List(ctx, "alice"); len(todos) != 0 {
		t.Errorf("invalid todos stored: %+v", todos)
	}
}

func TestPageEscapesTitles(t *testing.T) {
	a, h := app()
	a.Store.Add(ctx, "bob", "<script>alert(1)</script>")
	if body := do(h, "GET", "/", "", "", as("bob")).Body.String(); strings.Contains(body, "<script>alert") {
		t.Error("a title reached the page unescaped")
	}
}

func TestAPI(t *testing.T) {
	_, h := app()
	w := do(h, "POST", "/api/todos", `{"title":"water plants"}`, "application/json", as("bob"))
	if w.Code != http.StatusCreated {
		t.Fatalf("api add: %d %s", w.Code, w.Body)
	}
	if w := do(h, "POST", "/api/todos", `{"title":""}`, "application/json", as("bob")); w.Code != http.StatusBadRequest {
		t.Errorf("empty title: %d", w.Code)
	}
	var todos []Todo
	json.NewDecoder(do(h, "GET", "/api/todos", "", "", as("bob")).Body).Decode(&todos)
	if len(todos) != 1 || todos[0].Title != "water plants" || todos[0].CreatedBy != "bob" {
		t.Errorf("api list = %+v", todos)
	}
}

func TestOtherSitesCannotChangeTodos(t *testing.T) {
	a, h := app()
	w := do(h, "POST", "/todos", form(url.Values{"title": {"x"}}), "application/x-www-form-urlencoded", map[string]string{"Origin": "https://evil.example", userHeader: "alice"})
	if w.Code != http.StatusForbidden {
		t.Errorf("cross-site add: %d", w.Code)
	}
	if todos, _ := a.Store.List(ctx, "alice"); len(todos) != 0 {
		t.Error("a cross-site request added a todo")
	}
}

func TestEmptyListIsAnArray(t *testing.T) {
	_, h := app()
	if body := strings.TrimSpace(do(h, "GET", "/api/todos", "", "", as("alice")).Body.String()); body != "[]" {
		t.Errorf("empty list = %s", body)
	}
}

func TestHealth(t *testing.T) {
	_, h := app()
	if w := do(h, "GET", "/healthz", "", "", nil); w.Code != 200 {
		t.Errorf("healthz: %d", w.Code)
	}
}
