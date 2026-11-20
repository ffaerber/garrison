package events

import (
	"context"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

// Memory keeps events in memory, for tests and for running without a database.
type Memory struct {
	mu     sync.Mutex
	events []Event
	Now    func() time.Time
}

func (m *Memory) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

func (m *Memory) Record(_ context.Context, e Event) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e = e.clean(m.now())
	if e.Key != "" {
		for i, old := range m.events {
			if old.Key == e.Key {
				m.events[i] = merge(old, e)
				return
			}
		}
	}
	e.ID = int64(len(m.events) + 1)
	m.events = append(m.events, e)
}

// merge is what a second record with the same key changes.
func merge(old, e Event) Event {
	old.Status, old.Updated = e.Status, e.Updated
	if e.Title != "" {
		old.Title = e.Title
	}
	if e.Detail != "" {
		old.Detail = e.Detail
	}
	if e.Link != "" {
		old.Link = e.Link
	}
	if e.PR != 0 {
		old.PR = e.PR
	}
	if e.Commit != "" {
		old.Commit = e.Commit
	}
	return old
}

// All returns every event, oldest first.
func (m *Memory) All() []Event {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.events)
}

func (m *Memory) List(_ context.Context, q Query) ([]Event, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Event
	for i := len(m.events) - 1; i >= 0; i-- {
		e := m.events[i]
		if q.matches(e) {
			out = append(out, e)
		}
		if len(out) == q.limit() {
			break
		}
	}
	return out, nil
}

func (m *Memory) Get(_ context.Context, id int64, v Visibility) (Event, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, e := range m.events {
		if e.ID == id && v.Sees(e) {
			return e, true, nil
		}
	}
	return Event{}, false, nil
}

func (m *Memory) Related(ctx context.Context, e Event, v Visibility) ([]Event, error) {
	m.mu.Lock()
	all := slices.Clone(m.events)
	m.mu.Unlock()
	t := threadOf(e)
	var first []Event
	for _, o := range all {
		if v.Sees(o) && t.has(o) {
			first = append(first, o)
		}
	}
	t = t.widen(first)
	var out []Event
	for _, o := range all {
		if v.Sees(o) && t.has(o) {
			out = append(out, o)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out, nil
}

func (m *Memory) Units(_ context.Context, v Visibility) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	seen := map[string]bool{}
	var out []string
	for _, e := range m.events {
		if e.Unit != "" && !seen[e.Unit] && v.Sees(e) {
			seen[e.Unit] = true
			out = append(out, e.Unit)
		}
	}
	sort.Strings(out)
	return out, nil
}

// matches applies q's filters and visibility to e.
func (q Query) matches(e Event) bool {
	switch {
	case !q.Visibility.Sees(e):
		return false
	case q.Before > 0 && e.ID >= q.Before:
		return false
	case q.After > 0 && e.ID <= q.After:
		return false
	case q.Unit != "" && e.Unit != q.Unit:
		return false
	case len(q.Kinds) > 0 && !slices.Contains(q.Kinds, e.Kind):
		return false
	case len(q.Sources) > 0 && !slices.Contains(q.Sources, e.Source):
		return false
	case q.Problems && e.Status != StatusFailed && e.Status != StatusRefused:
		return false
	case q.Actor != "" && e.Actor != q.Actor, q.Status != "" && e.Status != q.Status:
		return false
	case q.Text != "" && !strings.Contains(strings.ToLower(e.Title+" "+e.Actor+" "+e.Agent+" "+e.Repo), strings.ToLower(q.Text)):
		return false
	}
	return true
}
