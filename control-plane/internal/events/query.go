package events

import (
	"context"
	"strconv"
	"time"
)

// Store keeps events and answers the dashboard.
type Store interface {
	Recorder
	// List returns the newest events matching q, newest first.
	List(ctx context.Context, q Query) ([]Event, error)
	// Get returns one event, if v sees it.
	Get(ctx context.Context, id int64, v Visibility) (Event, bool, error)
	// Related returns what belongs to e's thread, oldest first: events of
	// its pull request and its commits, a routine's calls, and, one step
	// further, the pull requests and commits those name.
	Related(ctx context.Context, e Event, v Visibility) ([]Event, error)
	// Namespaces lists the namespaces of the events v sees.
	Namespaces(ctx context.Context, v Visibility) ([]string, error)
}

// Query filters the feed.
type Query struct {
	Visibility Visibility
	Namespace  string
	Kinds      []string
	Sources    []string
	Problems   bool   // only failed or refused
	Actor      string // only those for this person
	Status     string // only those with this status
	Text       string // in title, person, agent or repo
	Before     int64  // only events with a smaller ID: the next page
	After      int64  // only events with a greater ID: what is new
	Limit      int
}

func (q Query) limit() int {
	if q.Limit <= 0 || q.Limit > 500 {
		return 100
	}
	return q.Limit
}

// Visibility is what one person may see: events of the repos they may read,
// and those for the people of the namespaces they are in.
type Visibility struct {
	Person  string          // sees what they did, wherever
	All     bool            // root admins
	Repos   map[string]bool // namespace/repo
	Members map[string]bool // namespace
}

// Sees tells whether the person may see e.
func (v Visibility) Sees(e Event) bool {
	if v.All || e.Namespace == "" || (v.Person != "" && e.Actor == v.Person) {
		return true
	}
	if e.Audience == AudienceMembers {
		return v.Members[e.Namespace]
	}
	return v.Repos[e.Namespace+"/"+repoOf(e)]
}

func repoOf(e Event) string {
	if e.Repo == "" {
		return "config"
	}
	return e.Repo
}

// thread is what ties events together.
type thread struct {
	id        int64
	commits   map[string]bool // namespace/repo@commit
	prs       map[string]bool // namespace/repo#number
	startedBy string          // a routine's run: its calls ...
	from, to  time.Time       // ... while it ran
}

func commitKey(e Event) string { return e.Namespace + "/" + repoOf(e) + "@" + e.Commit }
func prKey(e Event) string     { return e.Namespace + "/" + repoOf(e) + "#" + strconv.Itoa(e.PR) }

func threadOf(e Event) thread {
	t := thread{id: e.ID, commits: map[string]bool{}, prs: map[string]bool{}}
	t.add(e)
	// A routine's run, or an agent's hand-off: what was done for it.
	if (e.Kind == KindRoutine || e.Kind == KindAsk) && e.StartedBy != "" {
		t.startedBy = e.StartedBy
		t.from = e.At.Add(-time.Second)
		t.to = e.Updated.Add(time.Minute)
		if e.Status == StatusRunning {
			t.to = e.At.Add(24 * time.Hour)
		}
	}
	return t
}

func (t *thread) add(e Event) {
	if e.Commit != "" {
		t.commits[commitKey(e)] = true
	}
	if e.PR != 0 {
		t.prs[prKey(e)] = true
	}
}

func (t thread) has(e Event) bool {
	switch {
	case e.ID == t.id:
		return true
	case e.Commit != "" && t.commits[commitKey(e)]:
		return true
	case e.PR != 0 && t.prs[prKey(e)]:
		return true
	case t.startedBy != "" && e.StartedBy == t.startedBy && !e.At.Before(t.from) && !e.At.After(t.to):
		return true
	}
	return false
}

// widen adds the commits and pull requests of found to t.
func (t thread) widen(found []Event) thread {
	for _, e := range found {
		t.add(e)
	}
	return t
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
