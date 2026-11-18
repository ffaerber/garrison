package gateway

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"garrison/control-plane/internal/changes"
)

// The dashboard's cards: pull requests in plain words, with what the
// person can do about them. Everything is read and done as the person
// (Sudo), so Gitea's own rules decide what they see and may approve or
// merge; the dashboard adds no right of its own.

type giteaPull struct {
	Number    int       `json:"number"`
	Title     string    `json:"title"`
	Body      string    `json:"body"`
	State     string    `json:"state"`
	Merged    bool      `json:"merged"`
	HTMLURL   string    `json:"html_url"`
	CreatedAt time.Time `json:"created_at"`
	User      struct {
		Login string `json:"login"`
	} `json:"user"`
	Base struct {
		Ref string `json:"ref"`
	} `json:"base"`
	Head struct {
		Sha string `json:"sha"`
	} `json:"head"`
}

// Card is one pull request as person sees it; ErrForbidden if they may
// not read the repo, an error with status 404 if there is none.
func (g *Gitea) Card(ctx context.Context, person, ns, repo string, n int) (changes.Card, error) {
	if err := validOrg(ns); err != nil {
		return changes.Card{}, err
	}
	if err := validRepo(repo); err != nil {
		return changes.Card{}, err
	}
	base := fmt.Sprintf("/repos/%s/%s/pulls/%d", ns, repo, n)
	var p giteaPull
	if _, err := g.do(ctx, person, http.MethodGet, base, nil, &p); err != nil {
		return changes.Card{}, err
	}
	c := changes.Card{Namespace: ns, Repo: repo, Number: p.Number, Title: p.Title, Body: p.Body, Author: p.User.Login,
		Base: p.Base.Ref, URL: p.HTMLURL, Created: p.CreatedAt, Merged: p.Merged, Closed: p.State == "closed" && !p.Merged,
		Mine: p.User.Login == person}
	var files []struct {
		Filename string `json:"filename"`
		Status   string `json:"status"`
	}
	if _, err := g.do(ctx, person, http.MethodGet, base+"/files?limit=100", nil, &files); err != nil {
		return c, err
	}
	for _, f := range files {
		c.Files = append(c.Files, changes.File{Path: f.Filename, Status: f.Status})
	}
	if c.Open() {
		// Asked as the platform: people may not read the rules.
		var rule struct {
			Required int `json:"required_approvals"`
		}
		status, err := g.do(ctx, "", http.MethodGet, fmt.Sprintf("/repos/%s/%s/branch_protections/%s", ns, repo, escapePath(c.Base)), nil, &rule)
		switch {
		case status == http.StatusNotFound:
			c.NoApproval = true
		case err != nil:
			return c, err
		default:
			c.NoApproval = rule.Required == 0
		}
	}
	approvals, err := g.approvals(ctx, person, base)
	if err != nil {
		return c, err
	}
	c.Approvals = approvals
	for _, a := range approvals {
		if a == person {
			c.Approved = true
		}
	}
	if p.Head.Sha != "" {
		var st struct {
			Statuses []struct {
				Context     string `json:"context"`
				Status      string `json:"status"`
				Description string `json:"description"`
			} `json:"statuses"`
		}
		if _, err := g.do(ctx, person, http.MethodGet, fmt.Sprintf("/repos/%s/%s/commits/%s/status", ns, repo, url.PathEscape(p.Head.Sha)), nil, &st); err != nil {
			return c, err
		}
		for _, s := range st.Statuses {
			c.Checks = append(c.Checks, changes.Check{Name: s.Context, State: s.Status, Detail: s.Description})
		}
	}
	return c, nil
}

// approvals are who approve the pull request now: an approval later
// dismissed, made stale by a new commit or followed by a request for
// changes does not count.
func (g *Gitea) approvals(ctx context.Context, person, pull string) ([]string, error) {
	var reviews []struct {
		State     string `json:"state"`
		Stale     bool   `json:"stale"`
		Dismissed bool   `json:"dismissed"`
		User      struct {
			Login string `json:"login"`
		} `json:"user"`
	}
	if _, err := g.do(ctx, person, http.MethodGet, pull+"/reviews?limit=50", nil, &reviews); err != nil {
		return nil, err
	}
	last := map[string]bool{}
	var order []string
	for _, r := range reviews {
		switch r.State {
		case "APPROVED":
			if _, seen := last[r.User.Login]; !seen {
				order = append(order, r.User.Login)
			}
			last[r.User.Login] = !r.Stale && !r.Dismissed
		case "REQUEST_CHANGES":
			last[r.User.Login] = false
		}
	}
	var out []string
	for _, who := range order {
		if last[who] {
			out = append(out, who)
		}
	}
	return out, nil
}

// Waiting lists the open pull requests person could act on: theirs, and
// others' in the namespaces they are an admin of that they have not
// approved yet; at most limit.
func (g *Gitea) Waiting(ctx context.Context, person string, limit int) ([]changes.Card, error) {
	var issues []struct {
		Number     int `json:"number"`
		Repository struct {
			Owner string `json:"owner"`
			Name  string `json:"name"`
		} `json:"repository"`
		User struct {
			Login string `json:"login"`
		} `json:"user"`
	}
	if _, err := g.do(ctx, person, http.MethodGet, "/repos/issues/search?state=open&type=pulls&limit=50", nil, &issues); err != nil {
		return nil, err
	}
	// Which of them are theirs to act on: their own, and others' where
	// they are an admin.
	type pick struct {
		ns, repo string
		n        int
	}
	roles := map[string]string{}
	var picks []pick
	for _, is := range issues {
		ns, repo := is.Repository.Owner, is.Repository.Name
		if is.User.Login != person {
			role, seen := roles[ns]
			if !seen {
				r, err := g.Role(ctx, ns, person)
				if err != nil {
					return nil, err
				}
				roles[ns], role = r, r
			}
			if role != "admin" {
				continue
			}
		}
		picks = append(picks, pick{ns, repo, is.Number})
		if len(picks) >= 2*limit {
			break // enough, even with some approved already
		}
	}
	// Their cards, a few at a time: each is several requests to Gitea, and
	// the dashboard waits for them.
	cards := make([]changes.Card, len(picks))
	errs := make([]error, len(picks))
	var wg sync.WaitGroup
	slots := make(chan struct{}, 8)
	for i, p := range picks {
		wg.Add(1)
		go func() {
			defer wg.Done()
			slots <- struct{}{}
			defer func() { <-slots }()
			cards[i], errs[i] = g.Card(ctx, person, p.ns, p.repo, p.n)
		}()
	}
	wg.Wait()
	var out []changes.Card
	for i, c := range cards {
		if len(out) >= limit {
			break
		}
		if errs[i] != nil {
			return out, errs[i]
		}
		if !c.Mine && c.Approved {
			continue // done their part
		}
		out = append(out, c)
	}
	return out, nil
}

// Approve approves a pull request as person.
func (g *Gitea) Approve(ctx context.Context, person, ns, repo string, n int) error {
	if err := validOrg(ns); err != nil {
		return err
	}
	if err := validRepo(repo); err != nil {
		return err
	}
	_, err := g.do(ctx, person, http.MethodPost, fmt.Sprintf("/repos/%s/%s/pulls/%d/reviews", ns, repo, n),
		map[string]string{"event": "APPROVED", "body": "Approved on the dashboard."}, nil)
	return err
}

// Merge merges a pull request as person. Gitea asks to try again while it
// still checks a pull request, so it does, for a few seconds.
func (g *Gitea) Merge(ctx context.Context, person, ns, repo string, n int) error {
	if err := validOrg(ns); err != nil {
		return err
	}
	if err := validRepo(repo); err != nil {
		return err
	}
	var err error
	for i := 0; i < 5; i++ {
		_, err = g.do(ctx, person, http.MethodPost, fmt.Sprintf("/repos/%s/%s/pulls/%d/merge", ns, repo, n),
			map[string]string{"Do": "merge"}, nil)
		if err == nil || !strings.Contains(err.Error(), "try again later") {
			return err
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(2 * time.Second):
		}
	}
	return err
}
