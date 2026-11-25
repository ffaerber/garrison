package home

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"slices"

	"garrison/control-plane/internal/events"
	"garrison/control-plane/internal/secrets"
)

// Secrets is what the secrets page needs (secrets.Store): it writes
// values, and never reads one back.
type Secrets interface {
	Units(ctx context.Context, person string) ([]string, error)
	May(ctx context.Context, ns, person string) (bool, error)
	Names(ctx context.Context, ns string) ([]string, error)
	Set(ctx context.Context, ns, name, value, person string) error
	Delete(ctx context.Context, ns, name, person string) error
}

// secretsView is the secrets page: the units whose secrets the person
// manages, and the names of the one shown.
type secretsView struct {
	Units  []string
	NS     string
	Names  []string
	Name   string // the name the form had, after an error
	Notice string
	Error  string
}

func (s *Server) secretsView(ctx context.Context, person, ns string) (*secretsView, error) {
	v := &secretsView{}
	nss, err := s.Secrets.Units(ctx, person)
	if err != nil {
		return nil, err
	}
	v.Units = nss
	if len(nss) == 0 {
		return v, nil
	}
	if !slices.Contains(nss, ns) {
		ns = nss[0]
	}
	v.NS = ns
	v.Names, err = s.Secrets.Names(ctx, ns)
	return v, err
}

func (s *Server) secretsPage(w http.ResponseWriter, r *http.Request, v events.Visibility) {
	if s.Secrets == nil {
		http.NotFound(w, r)
		return
	}
	view, err := s.secretsView(r.Context(), v.Person, r.URL.Query().Get("ns"))
	if err != nil {
		log.Printf("home: secrets of %s: %v", v.Person, err)
		http.Error(w, "the secrets cannot be listed right now; try again", http.StatusServiceUnavailable)
		return
	}
	// What was just done, from the redirect after it: only names that can
	// be secrets are shown.
	if name := r.URL.Query().Get("name"); secrets.Name.MatchString(name) {
		switch r.URL.Query().Get("done") {
		case "set":
			view.Notice = fmt.Sprintf("Saved %s. Apps that use it get the new value as they redeploy, in a minute.", name)
		case "deleted":
			view.Notice = fmt.Sprintf("Deleted %s.", name)
		}
	}
	s.render(w, "page", feedPage{Who: v.Person, Domain: s.Domain, All: v.All, Now: s.now(), Secrets: view})
}

// secretsPost sets or deletes a secret: action=set with name and value, or
// action=delete with name.
func (s *Server) secretsPost(w http.ResponseWriter, r *http.Request, v events.Visibility) {
	if s.Secrets == nil {
		http.NotFound(w, r)
		return
	}
	// Apps live on the installation's subdomains, the same site as the
	// dashboard, so the sign-in's cookie comes along with a form any of
	// them posts here: only the dashboard's own pages may post.
	if r.Header.Get("Origin") != "https://"+s.Domain {
		http.Error(w, "this form is only taken from the dashboard's own page", http.StatusForbidden)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, secrets.MaxValue+16<<10)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "the value is too long", http.StatusRequestEntityTooLarge)
		return
	}
	ns, name, action := r.PathValue("ns"), r.PostForm.Get("name"), r.PostForm.Get("action")
	var err error
	var done, title string
	switch action {
	case "set":
		err = s.Secrets.Set(r.Context(), ns, name, r.PostForm.Get("value"), v.Person)
		done, title = "set", fmt.Sprintf("%s set the secret %s of %s", v.Person, name, ns)
	case "login":
		// A site's login: secret login-<name>, typed into the site's pages
		// by the gateway's browser for the agents logins.yaml names.
		login := r.PostForm.Get("login")
		name = secrets.LoginPrefix + login
		var value string
		if !secrets.LoginName.MatchString(login) {
			err = fmt.Errorf("%w: a login's name is lowercase letters, digits and dashes, at most 26", secrets.ErrInvalid)
		} else if value, err = secrets.NewLogin(r.PostForm.Get("site"), r.PostForm.Get("username"), r.PostForm.Get("password")); err == nil {
			err = s.Secrets.Set(r.Context(), ns, name, value, v.Person)
		}
		done, title = "set", fmt.Sprintf("%s set the login %s of %s", v.Person, login, ns)
	case "delete":
		err = s.Secrets.Delete(r.Context(), ns, name, v.Person)
		done, title = "deleted", fmt.Sprintf("%s deleted the secret %s of %s", v.Person, name, ns)
	default:
		http.Error(w, "unknown action", http.StatusBadRequest)
		return
	}
	if err != nil {
		status := http.StatusServiceUnavailable
		switch {
		case errors.Is(err, secrets.ErrForbidden):
			status = http.StatusForbidden
		case errors.Is(err, secrets.ErrInvalid):
			status = http.StatusBadRequest
		default:
			log.Printf("home: %s: %s %s/%s: %v", v.Person, action, ns, name, err)
		}
		view, verr := s.secretsView(r.Context(), v.Person, ns)
		if verr != nil {
			http.Error(w, err.Error(), status)
			return
		}
		view.Error, view.Name = err.Error(), name
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(status)
		s.render(w, "page", feedPage{Who: v.Person, Domain: s.Domain, All: v.All, Now: s.now(), Secrets: view})
		return
	}
	s.Events.Record(r.Context(), events.Event{Source: events.SourceHome, Kind: events.KindSecret, Status: events.StatusOK,
		Unit: ns, Repo: secrets.ConfigRepo, Audience: events.AudienceMembers, Actor: v.Person, StartedBy: v.Person, Title: title,
		Link: "https://" + s.Domain + "/secrets?ns=" + url.QueryEscape(ns)})
	http.Redirect(w, r, "/secrets?"+url.Values{"ns": {ns}, "done": {done}, "name": {name}}.Encode(), http.StatusSeeOther)
}
