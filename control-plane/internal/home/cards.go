package home

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

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
	case "merge", "approve_merge":
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
	case "approve_merge":
		// One click for the common case: the person reads the card, agrees,
		// and wants it in. The approval still counts only if it is theirs to
		// give; the merge waits for the checks it starts.
		if err = s.Changes.Approve(r.Context(), v.Person, ns, repo, n); err != nil {
			break
		}
		s.record(r, ns, repo, n, v.Person, fmt.Sprintf("%s approved %s/%s#%d on the dashboard", v.Person, ns, repo, n))
		if err = s.mergeSoon(r.Context(), v.Person, ns, repo, n); err != nil {
			log.Printf("home: %s: merge %s/%s#%d after approving: %v", v.Person, ns, repo, n, err)
			w.WriteHeader(http.StatusConflict)
			s.showCard(w, r, v, ns, repo, n, &cardView{Notice: "You approved it.", Error: changes.Plain(err) + " Merge it once it can be."})
			return
		}
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
	s.record(r, ns, repo, n, v.Person, title)
	http.Redirect(w, r, changes.Path(ns, repo, n)+"?done="+action, http.StatusSeeOther)
}

// record says in the feed that it was done here; Gitea's own webhook
// records the review and the merge themselves.
func (s *Server) record(r *http.Request, ns, repo string, n int, person, title string) {
	if s.Events != nil {
		s.Events.Record(r.Context(), events.Event{Source: events.SourceHome, Kind: events.KindPRUpdated, Status: events.StatusOK,
			Namespace: ns, Repo: repo, PR: n, Actor: person, Audience: events.AudienceMembers, Title: title})
	}
}

// mergeSoon merges what was just approved. The approval starts checks
// (garrison/approval is posted again within seconds), so a refusal for
// missing approvals or checks is tried again for a while.
func (s *Server) mergeSoon(ctx context.Context, person, ns, repo string, n int) error {
	wait := s.MergeWait
	if wait == 0 {
		wait = 40 * time.Second
	}
	step := min(2*time.Second, wait/10)
	deadline := time.Now().Add(wait)
	for {
		err := s.Changes.Merge(ctx, person, ns, repo, n)
		if err == nil || time.Now().After(deadline) {
			return err
		}
		m := err.Error()
		if !strings.Contains(m, "enough approvals") && !strings.Contains(m, "status check") && !strings.Contains(m, "try again later") {
			return err
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(step):
		}
	}
}
