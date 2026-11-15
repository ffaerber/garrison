package gateway

// Who pushed to a branch. A pull request names only who opened it; anyone
// who may write to the repo can push more onto its branch. Gitea's activity
// feed of a repo has every push, by whom and to which branch, and no push
// or force push takes an entry out again, unlike the pull request's own
// timeline.

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"slices"
	"strings"
	"sync"
)

// pushLog is what the feed of one repo has said so far.
type pushLog struct {
	last    int64               // the newest entry read
	pushers map[string][]string // branch -> who pushed to it since it was made
}

type pushLogs struct {
	mu   sync.Mutex
	logs map[string]*pushLog // by <org>/<repo>
}

// maxFeedPages bounds the first read of a repo's feed: 50 000 entries.
const maxFeedPages = 1000

// Pushers tells who pushed to each branch of a repo since the branch was
// made, as the admin sees the repo's feed. The first call reads the whole
// feed; later ones only what is new.
func (g *Gitea) Pushers(ctx context.Context, org, repo string) (map[string][]string, error) {
	if err := validOrg(org); err != nil {
		return nil, err
	}
	if err := validRepo(repo); err != nil {
		return nil, err
	}
	g.pushes.mu.Lock()
	defer g.pushes.mu.Unlock()
	if g.pushes.logs == nil {
		g.pushes.logs = map[string]*pushLog{}
	}
	key := org + "/" + repo
	l := g.pushes.logs[key]
	if l == nil {
		l = &pushLog{pushers: map[string][]string{}}
	}
	type entry struct {
		ID   int64  `json:"id"`
		Op   string `json:"op_type"`
		Ref  string `json:"ref_name"`
		User *struct {
			Login string `json:"login"`
		} `json:"act_user"`
	}
	// Newest first, until a page holds nothing new.
	var fresh []entry
	seen := map[int64]bool{}
	for page := 1; ; page++ {
		if page > maxFeedPages {
			log.Printf("pushes: %s has more than %d pages of activity; its oldest pushes are not counted", key, maxFeedPages)
			break
		}
		var batch []entry
		if _, err := g.do(ctx, "", http.MethodGet, fmt.Sprintf("/repos/%s/%s/activities/feeds?limit=50&page=%d", org, repo, page), nil, &batch); err != nil {
			return nil, fmt.Errorf("who pushed to %s: %w", key, err)
		}
		added := 0
		for _, e := range batch {
			if e.ID > l.last && !seen[e.ID] {
				seen[e.ID] = true
				fresh = append(fresh, e)
				added++
			}
		}
		if added == 0 || len(batch) < 50 {
			break
		}
	}
	// In the order they happened: a branch deleted and made again starts anew.
	slices.SortFunc(fresh, func(a, b entry) int { return int(a.ID - b.ID) })
	for _, e := range fresh {
		l.last = max(l.last, e.ID)
		branch, ok := strings.CutPrefix(e.Ref, "refs/heads/")
		if !ok {
			if strings.HasPrefix(e.Ref, "refs/") {
				continue // a tag
			}
			branch = e.Ref
		}
		switch e.Op {
		case "delete_branch":
			delete(l.pushers, branch)
		case "commit_repo":
			if e.User != nil && e.User.Login != "" && !slices.Contains(l.pushers[branch], e.User.Login) {
				l.pushers[branch] = append(l.pushers[branch], e.User.Login)
			}
		}
	}
	// Kept only once read whole: a failed read starts again.
	g.pushes.logs[key] = l
	out := make(map[string][]string, len(l.pushers))
	for b, who := range l.pushers {
		out[b] = slices.Clone(who)
	}
	return out, nil
}
