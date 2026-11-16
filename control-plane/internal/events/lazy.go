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
// are logged and dropped.
type Lazy struct {
	URL   string
	Retry time.Duration // 0: 30 seconds

	mu    sync.Mutex
	pg    *Postgres
	tried time.Time
}

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
		log.Printf("events: not recorded: %s %s/%s %q: %v", e.Kind, e.Namespace, e.Repo, e.Title, err)
		return
	}
	pg.Record(ctx, e)
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

func (l *Lazy) Namespaces(ctx context.Context, v Visibility) ([]string, error) {
	pg, err := l.get(ctx)
	if err != nil {
		return nil, err
	}
	return pg.Namespaces(ctx, v)
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
