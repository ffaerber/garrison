// Todo is a small todo app on Postgres, the example app of the namespace
// todo. Everyone has a list of their own. It runs behind the installation's
// sign-in (Keycloak, through the login proxy), which tells it who is signed
// in: each person sees and changes only their own todos.
package main

import (
	"context"
	"crypto/subtle"
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
	// IngressKey, if set, must come with every request of a person: see
	// ingressHeader.
	IngressKey string
}

func (a *App) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /{$}", a.signedIn(a.index))
	mux.Handle("POST /todos", a.signedIn(a.add))
	mux.Handle("POST /todos/{id}/toggle", a.signedIn(a.toggle))
	mux.Handle("POST /todos/{id}/delete", a.signedIn(a.remove))
	mux.Handle("GET /api/todos", a.signedIn(a.apiList))
	mux.Handle("POST /api/todos", a.signedIn(a.apiAdd))
	mux.HandleFunc("GET /healthz", a.health)
	return sameOrigin(mux)
}

// Signed-in person, as the installation's login check reports it. Traefik
// sets the header from the login proxy's answer, replacing any a client
// sent, so it names who signed in at Keycloak.
const userHeader = "X-Auth-Request-Preferred-Username"

// The environment's ingress key, which Traefik adds to every request it
// passes on after the sign-in. Other apps share Traefik's network and can
// call this one directly, naming anyone in userHeader; they do not know
// the key, which the platform gives this app as HOMECLOUD_INGRESS_KEY. So
// userHeader is believed only together with it.
const ingressHeader = "X-Homecloud-Ingress"

// signedIn serves only requests of a signed-in person that came through
// the sign-in, passing their name.
func (a *App) signedIn(h func(w http.ResponseWriter, r *http.Request, who string)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a.IngressKey != "" && subtle.ConstantTimeCompare([]byte(r.Header.Get(ingressHeader)), []byte(a.IngressKey)) != 1 {
			http.Error(w, "reach this app at its address, through the sign-in", http.StatusForbidden)
			return
		}
		who := r.Header.Get(userHeader)
		if who == "" {
			http.Error(w, "sign in first", http.StatusUnauthorized)
			return
		}
		h(w, r, who)
	})
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

func (a *App) index(w http.ResponseWriter, r *http.Request, who string) {
	todos, err := a.Store.List(r.Context(), who)
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
	page.Execute(w, map[string]any{"Todos": todos, "Open": open, "Who": who, "Error": r.URL.Query().Get("error")})
}

func (a *App) add(w http.ResponseWriter, r *http.Request, who string) {
	title, err := cleanTitle(r.FormValue("title"))
	if err == nil {
		_, err = a.Store.Add(r.Context(), who, title)
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

func (a *App) toggle(w http.ResponseWriter, r *http.Request, who string) {
	id, err := todoID(r)
	if err == nil {
		err = a.Store.Toggle(r.Context(), who, id)
	}
	if err != nil && !errors.Is(err, ErrNotFound) {
		log.Printf("toggle: %v", err)
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (a *App) remove(w http.ResponseWriter, r *http.Request, who string) {
	id, err := todoID(r)
	if err == nil {
		err = a.Store.Delete(r.Context(), who, id)
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

func (a *App) apiList(w http.ResponseWriter, r *http.Request, who string) {
	todos, err := a.Store.List(r.Context(), who)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "the database is not reachable"})
		return
	}
	if todos == nil {
		todos = []Todo{} // [] rather than null
	}
	writeJSON(w, http.StatusOK, todos)
}

func (a *App) apiAdd(w http.ResponseWriter, r *http.Request, who string) {
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
	t, err := a.Store.Add(r.Context(), who, title)
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
	app := &App{Store: store, IngressKey: os.Getenv("HOMECLOUD_INGRESS_KEY")}
	if app.IngressKey == "" {
		log.Printf("no HOMECLOUD_INGRESS_KEY: the sign-in's headers are believed from anyone who reaches this port")
	}
	srv := &http.Server{Addr: addr, Handler: app.Routes(), ReadHeaderTimeout: 10 * time.Second}
	log.Fatal(srv.ListenAndServe())
}
