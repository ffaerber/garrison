package events

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// While the database does not answer, events are kept, the oldest dropped
// beyond MaxPending, each with the time it happened.
func TestLazyKeepsEventsWhileTheDatabaseIsAway(t *testing.T) {
	pendingRetry = time.Hour // no retry during the test
	l := &Lazy{URL: "postgres://nobody@127.0.0.1:1/none?connect_timeout=1"}
	ctx := context.Background()
	before := time.Now()
	l.Record(ctx, Event{Kind: KindMedia, Title: "first"})
	l.mu.Lock()
	if len(l.pending) != 1 || l.pending[0].At.Before(before) || !l.retrying {
		t.Errorf("kept: %+v retrying %v", l.pending, l.retrying)
	}
	l.mu.Unlock()
	for i := 0; i < MaxPending; i++ {
		l.Record(ctx, Event{Kind: KindMedia, Title: fmt.Sprint(i)})
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.pending) != MaxPending || l.pending[0].Title != "0" || l.pending[MaxPending-1].Title != fmt.Sprint(MaxPending-1) {
		t.Errorf("%d kept, first %q", len(l.pending), l.pending[0].Title)
	}
}
