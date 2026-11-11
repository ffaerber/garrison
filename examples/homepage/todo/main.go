// Todo is a small todo list on Postgres, the example app of the namespace
// homepage. It runs behind the installation's sign-in, which tells it who is
// signed in; it keeps who added each todo.
package main

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

//go:embed page.html
var pages embed.FS

var page = template.Must(template.ParseFS(pages, "page.html"))

const maxTitle = 200

// App serves the todo list over a Store.
type App struct {
	Store Store
}

func (a *App) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", a.index)
	mux.HandleFunc("POST /todos", a.add)
	mux.HandleFunc("POST /todos/{id}/toggle", a.toggle)
	mux.HandleFunc("POST /todos/{id}/delete", a.remove)
	mux.HandleFunc("GET /api/todos", a.apiList)
	mux.HandleFunc("POST /api/todos", a.apiAdd)
	mux.HandleFunc("GET /healthz", a.health)
	return sameOrigin(mux)
}

// who is the signed-in person, as the installation's login check reports it.
func who(r *http.Request) string {
	if u := r.Header.Get("X-Auth-Request-Preferred-Username"); u != "" {
		return u
	}
	return "someone"
}

// sameOrigin refuses changes sent from other sites.
func sameOrigin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if o := r.Header.Get("Origin"); o != "" {
				if u, err := url.Parse(o); err != nil || u.Host != r.Host {
					http.Error(w, "cross-site request refused", http.StatusForbidden)
					return
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}

func cleanTitle(s string) (string, error) {
	s = strings.TrimSpace(s)
	switch {
	case s == "":
		return "", errors.New("a todo needs a title")
	case len(s) > maxTitle:
		return "", errors.New("a title has at most 200 characters")
	}
	return s, nil
}

func (a *App) index(w http.ResponseWriter, r *http.Request) {
	todos, err := a.Store.List(r.Context())
	if err != nil {
		log.Printf("list: %v", err)
		http.Error(w, "the database is not reachable", http.StatusServiceUnavailable)
		return
	}
	open := 0
	for _, t := range todos {
		if !t.Done {
			open++
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	page.Execute(w, map[string]any{"Todos": todos, "Open": open, "Who": who(r), "Error": r.URL.Query().Get("error")})
}

func (a *App) add(w http.ResponseWriter, r *http.Request) {
	title, err := cleanTitle(r.FormValue("title"))
	if err == nil {
		_, err = a.Store.Add(r.Context(), title, who(r))
	}
	target := "/"
	if err != nil {
		target = "/?error=" + url.QueryEscape(err.Error())
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

func todoID(r *http.Request) (int64, error) {
	return strconv.ParseInt(r.PathValue("id"), 10, 64)
}

func (a *App) toggle(w http.ResponseWriter, r *http.Request) {
	id, err := todoID(r)
	if err == nil {
		err = a.Store.Toggle(r.Context(), id)
	}
	if err != nil && !errors.Is(err, ErrNotFound) {
		log.Printf("toggle: %v", err)
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (a *App) remove(w http.ResponseWriter, r *http.Request) {
	id, err := todoID(r)
	if err == nil {
		err = a.Store.Delete(r.Context(), id)
	}
	if err != nil && !errors.Is(err, ErrNotFound) {
		log.Printf("delete: %v", err)
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func (a *App) apiList(w http.ResponseWriter, r *http.Request) {
	todos, err := a.Store.List(r.Context())
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "the database is not reachable"})
		return
	}
	if todos == nil {
		todos = []Todo{} // [] rather than null
	}
	writeJSON(w, http.StatusOK, todos)
}

func (a *App) apiAdd(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Title string `json:"title"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": `send {"title": "..."}`})
		return
	}
	title, err := cleanTitle(in.Title)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	t, err := a.Store.Add(r.Context(), title, who(r))
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "the database is not reachable"})
		return
	}
	writeJSON(w, http.StatusCreated, t)
}

func (a *App) health(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := a.Store.Ping(ctx); err != nil {
		http.Error(w, "database: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	w.Write([]byte("ok"))
}

func main() {
	password, err := os.ReadFile(os.Getenv("DATABASE_PASSWORD_FILE"))
	if err != nil {
		log.Fatalf("database password: %v", err)
	}
	dsn := (&url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword(os.Getenv("DATABASE_USER"), strings.TrimSpace(string(password))),
		Host:     os.Getenv("DATABASE_HOST") + ":5432",
		Path:     os.Getenv("DATABASE_NAME"),
		RawQuery: "sslmode=disable",
	}).String()
	ctx := context.Background()
	store, err := OpenPostgres(ctx, dsn)
	if err != nil {
		log.Fatalf("database: %v", err)
	}
	addr := os.Getenv("LISTEN")
	if addr == "" {
		addr = ":8080"
	}
	log.Printf("todo on %s", addr)
	srv := &http.Server{Addr: addr, Handler: (&App{Store: store}).Routes(), ReadHeaderTimeout: 10 * time.Second}
	log.Fatal(srv.ListenAndServe())
}
