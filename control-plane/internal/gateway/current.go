package gateway

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
)

// keepCurrent brings the open pull requests of proposals (branches change/
// and agent/) up to date with their base branch when it has moved: Gitea
// merges the base into the branch, as the pull request's author. Where they
// conflict, it says so once in the pull request, for a person or the agent
// to resolve. So a proposal keeps showing what it would change today, and
// its preview runs on top of what the base has become.
func (m *Members) keepCurrent(ctx context.Context, ns string) error {
	g := m.Gitea
	if m.current == nil {
		m.current = map[string]string{}
	}
	repos, err := g.Repos(ctx, "", ns)
	if err != nil {
		return err
	}
	var errs []error
	for _, r := range repos {
		if r.Empty || r.Name == ConfigRepo {
			continue // config's changes wait for approval, which a new head would undo
		}
		var prs []struct {
			Number    int    `json:"number"`
			MergeBase string `json:"merge_base"`
			Mergeable bool   `json:"mergeable"`
			User      struct {
				Login string `json:"login"`
			} `json:"user"`
			Base struct {
				Ref string `json:"ref"`
				SHA string `json:"sha"`
			} `json:"base"`
			Head struct {
				Ref string `json:"ref"`
				SHA string `json:"sha"`
			} `json:"head"`
		}
		if _, err := g.do(ctx, "", http.MethodGet, fmt.Sprintf("/repos/%s/%s/pulls?state=open&limit=50", ns, r.Name), nil, &prs); err != nil {
			errs = append(errs, err)
			continue
		}
		for _, pr := range prs {
			if !strings.HasPrefix(pr.Head.Ref, ChangeBranch) && !strings.HasPrefix(pr.Head.Ref, "agent/") {
				continue
			}
			if pr.MergeBase == "" || pr.Base.SHA == "" || pr.MergeBase == pr.Base.SHA {
				continue // up to date
			}
			key := fmt.Sprintf("%s/%s#%d", ns, r.Name, pr.Number)
			state := pr.Head.SHA + " " + pr.Base.SHA
			if m.current[key] == state {
				continue // tried already, for these commits
			}
			m.current[key] = state
			path := fmt.Sprintf("/repos/%s/%s/pulls/%d", ns, r.Name, pr.Number)
			status, err := g.do(ctx, pr.User.Login, http.MethodPost, path+"/update?style=merge", nil, nil)
			switch {
			case err == nil:
				log.Printf("changes: brought %s up to date with %s", key, pr.Base.Ref)
			case status == http.StatusConflict || (!pr.Mergeable && status >= 400 && status < 500 && !errors.Is(err, ErrForbidden)):
				g.do(ctx, "", http.MethodPost, fmt.Sprintf("/repos/%s/%s/issues/%d/comments", ns, r.Name, pr.Number), map[string]string{
					"body": fmt.Sprintf("%s has moved on (now %.12s) and this change conflicts with it, so it was not brought up to date. Ask the agent that proposed it, or resolve it in a checkout: merge %s into %s.", pr.Base.Ref, pr.Base.SHA, pr.Base.Ref, pr.Head.Ref),
				}, nil)
				log.Printf("changes: %s conflicts with %s; said so in it", key, pr.Base.Ref)
			default:
				log.Printf("changes: cannot bring %s up to date as %s: %v", key, pr.User.Login, err)
			}
		}
	}
	return errors.Join(errs...)
}
