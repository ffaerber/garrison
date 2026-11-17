package home

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"

	"garrison/control-plane/internal/changes"
	"garrison/control-plane/internal/events"
)

// Changes reads and acts on pull requests as a person (gateway.Gitea).
type Changes interface {
	Card(ctx context.Context, person, ns, repo string, n int) (changes.Card, error)
	Waiting(ctx context.Context, person string, limit int) ([]changes.Card, error)
	Approve(ctx context.Context, person, ns, repo string, n int) error
	Merge(ctx context.Context, person, ns, repo string, n int) error
}

// cardView is the page of one change.
type cardView struct {
	Card   changes.Card
	Notice string
	Error  string
}

func cardArgs(r *http.Request) (ns, repo string, n int, ok bool) {
	n, err := strconv.Atoi(r.PathValue("n"))
	return r.PathValue("ns"), r.PathValue("repo"), n, err == nil && n > 0
}

func (s *Server) cardPage(w http.ResponseWriter, r *http.Request, v events.Visibility) {
	ns, repo, n, ok := cardArgs(r)
	if !ok || s.Changes == nil {
		http.NotFound(w, r)
		return
	}
	view := &cardView{}
	switch r.URL.Query().Get("done") {
	case "approve":
		view.Notice = "You approved it."
	case "merge":
		view.Notice = "You merged it."
	}
	s.showCard(w, r, v, ns, repo, n, view)
}

func (s *Server) showCard(w http.ResponseWriter, r *http.Request, v events.Visibility, ns, repo string, n int, view *cardView) {
	c, err := s.Changes.Card(r.Context(), v.Person, ns, repo, n)
	if err != nil {
		// Not there and not theirs to read look the same.
		if errors.Is(err, changes.ErrForbidden) || strings.Contains(err.Error(), " 404 ") {
			http.NotFound(w, r)
			return
		}
		log.Printf("home: %s: card %s/%s#%d: %v", v.Person, ns, repo, n, err)
		http.Error(w, "the change cannot be read right now; try again", http.StatusServiceUnavailable)
		return
	}
	view.Card = c
	s.render(w, "page", feedPage{Who: v.Person, Domain: s.Domain, All: v.All, Now: s.now(), Card: view})
}

// cardPost approves or merges, as the person: Gitea decides whether they
// may.
func (s *Server) cardPost(w http.ResponseWriter, r *http.Request, v events.Visibility) {
	ns, repo, n, ok := cardArgs(r)
	if !ok || s.Changes == nil {
		http.NotFound(w, r)
		return
	}
	// As for secrets: apps on the same site may not post here for them.
	if r.Header.Get("Origin") != "https://"+s.Domain {
		http.Error(w, "this form is only taken from the dashboard's own page", http.StatusForbidden)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	action := r.PostForm.Get("action")
	var err error
	var title string
	switch action {
	case "approve":
		err = s.Changes.Approve(r.Context(), v.Person, ns, repo, n)
		title = fmt.Sprintf("%s approved %s/%s#%d on the dashboard", v.Person, ns, repo, n)
	case "merge":
		err = s.Changes.Merge(r.Context(), v.Person, ns, repo, n)
		title = fmt.Sprintf("%s merged %s/%s#%d on the dashboard", v.Person, ns, repo, n)
	default:
		http.Error(w, "unknown action", http.StatusBadRequest)
		return
	}
	if err != nil {
		log.Printf("home: %s: %s %s/%s#%d: %v", v.Person, action, ns, repo, n, err)
		w.WriteHeader(http.StatusConflict)
		s.showCard(w, r, v, ns, repo, n, &cardView{Error: changes.Plain(err)})
		return
	}
	// Gitea's own webhook records the review and the merge in the feed; this
	// says it was done here.
	if s.Events != nil {
		s.Events.Record(r.Context(), events.Event{Source: events.SourceHome, Kind: events.KindPRUpdated, Status: events.StatusOK,
			Namespace: ns, Repo: repo, PR: n, Actor: v.Person, Audience: events.AudienceMembers, Title: title})
	}
	http.Redirect(w, r, changes.Path(ns, repo, n)+"?done="+action, http.StatusSeeOther)
}
