package main

import (
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

func TestAddToggleDelete(t *testing.T) {
	a, h := app()
	me := map[string]string{"X-Auth-Request-Preferred-Username": "alice"}
	if w := do(h, "POST", "/todos", form(url.Values{"title": {"  buy milk "}}), "application/x-www-form-urlencoded", me); w.Code != http.StatusSeeOther {
		t.Fatalf("add: %d", w.Code)
	}
	todos, _ := a.Store.List(nil)
	if len(todos) != 1 || todos[0].Title != "buy milk" || todos[0].CreatedBy != "alice" {
		t.Fatalf("todos = %+v", todos)
	}
	w := do(h, "GET", "/", "", "", me)
	if !strings.Contains(w.Body.String(), "buy milk") || !strings.Contains(w.Body.String(), "1 open · signed in as alice") {
		t.Errorf("page:\n%s", w.Body.String())
	}
	do(h, "POST", "/todos/1/toggle", "", "", nil)
	if todos, _ := a.Store.List(nil); !todos[0].Done {
		t.Error("toggle did not mark it done")
	}
	do(h, "POST", "/todos/1/delete", "", "", nil)
	if todos, _ := a.Store.List(nil); len(todos) != 0 {
		t.Error("delete did not remove it")
	}
}

func TestTitlesAreChecked(t *testing.T) {
	a, h := app()
	for _, title := range []string{"", "   ", strings.Repeat("x", 201)} {
		w := do(h, "POST", "/todos", form(url.Values{"title": {title}}), "application/x-www-form-urlencoded", nil)
		if loc := w.Header().Get("Location"); !strings.Contains(loc, "error=") {
			t.Errorf("title %q accepted: %s", title, loc)
		}
	}
	if todos, _ := a.Store.List(nil); len(todos) != 0 {
		t.Errorf("invalid todos stored: %+v", todos)
	}
}

func TestPageEscapesTitles(t *testing.T) {
	a, h := app()
	a.Store.Add(nil, "<script>alert(1)</script>", "bob")
	if body := do(h, "GET", "/", "", "", nil).Body.String(); strings.Contains(body, "<script>alert") {
		t.Error("a title reached the page unescaped")
	}
}

func TestAPI(t *testing.T) {
	_, h := app()
	w := do(h, "POST", "/api/todos", `{"title":"water plants"}`, "application/json", map[string]string{"X-Auth-Request-Preferred-Username": "bob"})
	if w.Code != http.StatusCreated {
		t.Fatalf("api add: %d %s", w.Code, w.Body)
	}
	if w := do(h, "POST", "/api/todos", `{"title":""}`, "application/json", nil); w.Code != http.StatusBadRequest {
		t.Errorf("empty title: %d", w.Code)
	}
	var todos []Todo
	json.NewDecoder(do(h, "GET", "/api/todos", "", "", nil).Body).Decode(&todos)
	if len(todos) != 1 || todos[0].Title != "water plants" || todos[0].CreatedBy != "bob" {
		t.Errorf("api list = %+v", todos)
	}
}

func TestOtherSitesCannotChangeTodos(t *testing.T) {
	a, h := app()
	w := do(h, "POST", "/todos", form(url.Values{"title": {"x"}}), "application/x-www-form-urlencoded", map[string]string{"Origin": "https://evil.example"})
	if w.Code != http.StatusForbidden {
		t.Errorf("cross-site add: %d", w.Code)
	}
	if todos, _ := a.Store.List(nil); len(todos) != 0 {
		t.Error("a cross-site request added a todo")
	}
}

func TestEmptyListIsAnArray(t *testing.T) {
	_, h := app()
	if body := strings.TrimSpace(do(h, "GET", "/api/todos", "", "", nil).Body.String()); body != "[]" {
		t.Errorf("empty list = %s", body)
	}
}

func TestHealth(t *testing.T) {
	_, h := app()
	if w := do(h, "GET", "/healthz", "", "", nil); w.Code != 200 {
		t.Errorf("healthz: %d", w.Code)
	}
}
