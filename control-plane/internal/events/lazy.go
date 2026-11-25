package events

import (
	"context"
	"errors"
	"log"
	"sync"
	"time"
)

// Lazy is a Postgres store that connects when first used, and again after
// a failure, at most every Retry: the services start before Postgres may
// answer, and must not wait for it. Events recorded while it cannot connect
// are kept, up to MaxPending, and written once it answers: an agent's work
// right after a restart was otherwise missing from the feed.
type Lazy struct {
	URL   string
	Retry time.Duration // 0: 30 seconds

	mu       sync.Mutex
	pg       *Postgres
	tried    time.Time
	pending  []Event
	retrying bool
}

// MaxPending is how many events Lazy keeps while the database is away; the
// oldest go first.
const MaxPending = 1000

// pendingRetry is how often kept events try the database again.
var pendingRetry = 5 * time.Second

var errNotConnected = errors.New("the events database is not reachable yet")

func (l *Lazy) get(ctx context.Context) (*Postgres, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.pg != nil {
		return l.pg, nil
	}
	retry := l.Retry
	if retry == 0 {
		retry = 30 * time.Second
	}
	if time.Since(l.tried) < retry {
		return nil, errNotConnected
	}
	l.tried = time.Now()
	pg, err := Open(ctx, l.URL, 0)
	if err != nil {
		log.Printf("events: %v", err)
		return nil, errNotConnected
	}
	l.pg = pg
	return pg, nil
}

func (l *Lazy) Record(ctx context.Context, e Event) {
	pg, err := l.get(context.WithoutCancel(ctx))
	if err != nil {
		l.keep(e)
		return
	}
	l.flush(ctx, pg)
	pg.Record(ctx, e)
}

// keep holds e until the database answers, and starts trying it again.
func (l *Lazy) keep(e Event) {
	if e.At.IsZero() {
		e.At = time.Now() // when it happened, not when it is written
	}
	l.mu.Lock()
	if len(l.pending) >= MaxPending {
		old := l.pending[0]
		log.Printf("events: not recorded: %s %s/%s %q: more than %d wait for the database", old.Kind, old.Unit, old.Repo, old.Title, MaxPending)
		l.pending = l.pending[1:]
	}
	l.pending = append(l.pending, e)
	start := !l.retrying
	l.retrying = true
	l.mu.Unlock()
	log.Printf("events: kept until the database answers: %s %s/%s %q", e.Kind, e.Unit, e.Repo, e.Title)
	if start {
		go l.retry()
	}
}

// retry writes the kept events once the database answers.
func (l *Lazy) retry() {
	for {
		time.Sleep(pendingRetry)
		l.mu.Lock()
		l.tried = time.Time{} // try now, not after Retry
		l.mu.Unlock()
		if pg, err := l.get(context.Background()); err == nil {
			l.flush(context.Background(), pg)
			l.mu.Lock()
			done := len(l.pending) == 0
			l.retrying = !done
			l.mu.Unlock()
			if done {
				return
			}
		}
	}
}

// flush writes the kept events, oldest first.
func (l *Lazy) flush(ctx context.Context, pg *Postgres) {
	l.mu.Lock()
	kept := l.pending
	l.pending = nil
	l.mu.Unlock()
	for _, e := range kept {
		pg.Record(context.WithoutCancel(ctx), e)
	}
	if len(kept) > 0 {
		log.Printf("events: recorded %d kept while the database was away", len(kept))
	}
}

func (l *Lazy) List(ctx context.Context, q Query) ([]Event, error) {
	pg, err := l.get(ctx)
	if err != nil {
		return nil, err
	}
	return pg.List(ctx, q)
}

func (l *Lazy) Get(ctx context.Context, id int64, v Visibility) (Event, bool, error) {
	pg, err := l.get(ctx)
	if err != nil {
		return Event{}, false, err
	}
	return pg.Get(ctx, id, v)
}

func (l *Lazy) Related(ctx context.Context, e Event, v Visibility) ([]Event, error) {
	pg, err := l.get(ctx)
	if err != nil {
		return nil, err
	}
	return pg.Related(ctx, e, v)
}

func (l *Lazy) Units(ctx context.Context, v Visibility) ([]string, error) {
	pg, err := l.get(ctx)
	if err != nil {
		return nil, err
	}
	return pg.Units(ctx, v)
}

// Prune prunes once connected, every hour until ctx ends.
func (l *Lazy) Prune(ctx context.Context) {
	for {
		if pg, err := l.get(ctx); err == nil {
			pg.Prune(ctx)
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Minute):
		}
	}
}

func (l *Lazy) RoutineStates(ctx context.Context) (map[string]RoutineState, error) {
	pg, err := l.get(ctx)
	if err != nil {
		return nil, err
	}
	return pg.RoutineStates(ctx)
}

func (l *Lazy) DeleteRoutineState(ctx context.Context, key string) error {
	pg, err := l.get(ctx)
	if err != nil {
		return err
	}
	return pg.DeleteRoutineState(ctx, key)
}

func (l *Lazy) SaveRoutineState(ctx context.Context, key string, s RoutineState) error {
	pg, err := l.get(ctx)
	if err != nil {
		return err
	}
	return pg.SaveRoutineState(ctx, key, s)
}

func (l *Lazy) AddQuestion(ctx context.Context, q *Question) error {
	pg, err := l.get(ctx)
	if err != nil {
		return err
	}
	return pg.AddQuestion(ctx, q)
}

func (l *Lazy) Questions(ctx context.Context, status string) ([]Question, error) {
	pg, err := l.get(ctx)
	if err != nil {
		return nil, err
	}
	return pg.Questions(ctx, status)
}

func (l *Lazy) SetQuestion(ctx context.Context, q Question) error {
	pg, err := l.get(ctx)
	if err != nil {
		return err
	}
	return pg.SetQuestion(ctx, q)
}
