package events

import (
	"context"
	"fmt"
	"log"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Postgres keeps events in the table events of its database.
type Postgres struct {
	Pool *pgxpool.Pool
	// Keep is how long events are kept; 0: 90 days.
	Keep time.Duration
}

const schema = `
CREATE TABLE IF NOT EXISTS events (
  id         bigserial PRIMARY KEY,
  at         timestamptz NOT NULL,
  updated    timestamptz NOT NULL,
  source     text NOT NULL,
  kind       text NOT NULL,
  status     text NOT NULL,
  unit       text NOT NULL DEFAULT '',
  repo       text NOT NULL DEFAULT '',
  audience   text NOT NULL DEFAULT '',
  actor      text NOT NULL DEFAULT '',
  agent      text NOT NULL DEFAULT '',
  started_by text NOT NULL DEFAULT '',
  title      text NOT NULL,
  detail     text NOT NULL DEFAULT '',
  link       text NOT NULL DEFAULT '',
  commit     text NOT NULL DEFAULT '',
  pr         integer NOT NULL DEFAULT 0,
  key        text UNIQUE
);
-- Units were called namespaces before: an older table is renamed in place.
DO $$ BEGIN
  IF EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'events' AND column_name = 'namespace') THEN
    ALTER TABLE events RENAME COLUMN namespace TO unit;
    UPDATE events SET kind = 'unit' WHERE kind = 'namespace';
  END IF;
END $$;
CREATE INDEX IF NOT EXISTS events_by_unit ON events (unit, id);
CREATE INDEX IF NOT EXISTS events_by_commit ON events (unit, repo, commit) WHERE commit <> '';
CREATE INDEX IF NOT EXISTS events_by_pr ON events (unit, repo, pr) WHERE pr <> 0;
CREATE INDEX IF NOT EXISTS events_by_starter ON events (started_by, at) WHERE started_by <> '';
`

// Open connects to url (a postgres:// URL) and makes the table. Postgres
// may still be starting: it tries for up to wait.
func Open(ctx context.Context, url string, wait time.Duration) (*Postgres, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("events database: %w", err)
	}
	cfg.MaxConns = 4
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(wait)
	for {
		_, err = pool.Exec(ctx, schema+routineSchema+questionSchema+traceSchema+inviteSchema+shotSchema)
		if err == nil || time.Now().After(deadline) || ctx.Err() != nil {
			break
		}
		time.Sleep(2 * time.Second)
	}
	if err != nil {
		pool.Close()
		return nil, fmt.Errorf("events database: %w", err)
	}
	return &Postgres{Pool: pool}, nil
}

const columns = `id, at, updated, source, kind, status, unit, repo, audience, actor, agent, started_by, title, detail, link, commit, pr, coalesce(key, '')`

func scan(rows pgx.Rows) ([]Event, error) {
	var out []Event
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.ID, &e.At, &e.Updated, &e.Source, &e.Kind, &e.Status, &e.Unit, &e.Repo, &e.Audience,
			&e.Actor, &e.Agent, &e.StartedBy, &e.Title, &e.Detail, &e.Link, &e.Commit, &e.PR, &e.Key); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// Record keeps e. It waits at most a few seconds and logs a failure:
// the work it describes goes on.
func (p *Postgres) Record(ctx context.Context, e Event) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	e = e.clean(time.Now())
	var key any
	if e.Key != "" {
		key = e.Key
	}
	_, err := p.Pool.Exec(ctx, `
INSERT INTO events (at, updated, source, kind, status, unit, repo, audience, actor, agent, started_by, title, detail, link, commit, pr, key)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17)
ON CONFLICT (key) DO UPDATE SET
  status = excluded.status, updated = excluded.updated,
  title  = coalesce(nullif(excluded.title, ''), events.title),
  detail = coalesce(nullif(excluded.detail, ''), events.detail),
  link   = coalesce(nullif(excluded.link, ''), events.link),
  commit = coalesce(nullif(excluded.commit, ''), events.commit),
  pr     = coalesce(nullif(excluded.pr, 0), events.pr)`,
		e.At, e.Updated, e.Source, e.Kind, e.Status, e.Unit, e.Repo, e.Audience, e.Actor, e.Agent, e.StartedBy,
		e.Title, e.Detail, e.Link, e.Commit, e.PR, key)
	if err != nil {
		log.Printf("events: not recorded: %s %s/%s %q: %v", e.Kind, e.Unit, e.Repo, e.Title, err)
	}
}

// visible is the SQL condition for v, with its arguments from $n on.
func visible(v Visibility, n int) (string, []any) {
	if v.All {
		return "true", nil
	}
	repos := keys(v.Repos)
	members := keys(v.Members)
	return fmt.Sprintf(`(unit = '' OR (actor <> '' AND actor = $%d) OR (audience = '%s' AND unit = ANY($%d)) OR (audience <> '%s' AND unit || '/' || coalesce(nullif(repo, ''), 'config') = ANY($%d)))`,
		n, AudienceMembers, n+1, AudienceMembers, n+2), []any{v.Person, members, repos}
}

func (p *Postgres) List(ctx context.Context, q Query) ([]Event, error) {
	where, args := visible(q.Visibility, 1)
	arg := func(v any) string {
		args = append(args, v)
		return fmt.Sprintf("$%d", len(args))
	}
	if q.Unit != "" {
		where += " AND unit = " + arg(q.Unit)
	}
	if len(q.Kinds) > 0 {
		where += " AND kind = ANY(" + arg(q.Kinds) + ")"
	}
	if len(q.Sources) > 0 {
		where += " AND source = ANY(" + arg(q.Sources) + ")"
	}
	if q.Actor != "" {
		where += " AND actor = " + arg(q.Actor)
	}
	if q.Status != "" {
		where += " AND status = " + arg(q.Status)
	}
	if q.Problems {
		where += fmt.Sprintf(" AND status IN ('%s', '%s')", StatusFailed, StatusRefused)
	}
	if q.Text != "" {
		where += " AND strpos(lower(title || ' ' || actor || ' ' || agent || ' ' || repo), lower(" + arg(q.Text) + ")) > 0"
	}
	if q.Before > 0 {
		where += " AND id < " + arg(q.Before)
	}
	if q.After > 0 {
		where += " AND id > " + arg(q.After)
	}
	rows, err := p.Pool.Query(ctx, "SELECT "+columns+" FROM events WHERE "+where+" ORDER BY id DESC LIMIT "+arg(q.limit()), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scan(rows)
}

func (p *Postgres) Get(ctx context.Context, id int64, v Visibility) (Event, bool, error) {
	rows, err := p.Pool.Query(ctx, "SELECT "+columns+" FROM events WHERE id = $1", id)
	if err != nil {
		return Event{}, false, err
	}
	defer rows.Close()
	es, err := scan(rows)
	if err != nil || len(es) == 0 || !v.Sees(es[0]) {
		return Event{}, false, err
	}
	return es[0], true, nil
}

func (p *Postgres) thread(ctx context.Context, t thread) ([]Event, error) {
	rows, err := p.Pool.Query(ctx, "SELECT "+columns+` FROM events WHERE id = $1
  OR (commit <> '' AND unit || '/' || coalesce(nullif(repo, ''), 'config') || '@' || commit = ANY($2))
  OR (pr <> 0 AND unit || '/' || coalesce(nullif(repo, ''), 'config') || '#' || pr = ANY($3))
  OR ($4 <> '' AND started_by = $4 AND at BETWEEN $5 AND $6)
ORDER BY id LIMIT 500`, t.id, keys(t.commits), keys(t.prs), t.startedBy, t.from, t.to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scan(rows)
}

func (p *Postgres) Related(ctx context.Context, e Event, v Visibility) ([]Event, error) {
	t := threadOf(e)
	first, err := p.thread(ctx, t)
	if err != nil {
		return nil, err
	}
	all, err := p.thread(ctx, t.widen(first))
	if err != nil {
		return nil, err
	}
	var out []Event
	for _, o := range all {
		if v.Sees(o) {
			out = append(out, o)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out, nil
}

func (p *Postgres) Units(ctx context.Context, v Visibility) ([]string, error) {
	where, args := visible(v, 1)
	rows, err := p.Pool.Query(ctx, "SELECT DISTINCT unit FROM events WHERE unit <> '' AND "+where+" ORDER BY 1", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// Prune deletes events older than Keep, every hour until ctx ends.
func (p *Postgres) Prune(ctx context.Context) {
	keep := p.Keep
	if keep == 0 {
		keep = 90 * 24 * time.Hour
	}
	for {
		if _, err := p.Pool.Exec(ctx, "DELETE FROM events WHERE at < $1", time.Now().Add(-keep)); err != nil && ctx.Err() == nil {
			log.Printf("events: prune: %v", err)
		}
		p.PruneTraces(ctx)
		p.PruneInvites(ctx)
		p.PruneShots(ctx)
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Hour):
		}
	}
}

// RoutineState is what the agent service's scheduler keeps of a routine:
// when it last started, and the commit an on_push routine last saw.
type RoutineState struct {
	Last time.Time
	Head string
}

const routineSchema = `
CREATE TABLE IF NOT EXISTS routine_state (
  key  text PRIMARY KEY,
  last timestamptz,
  head text NOT NULL DEFAULT ''
);`

func (p *Postgres) RoutineStates(ctx context.Context) (map[string]RoutineState, error) {
	rows, err := p.Pool.Query(ctx, "SELECT key, last, head FROM routine_state")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]RoutineState{}
	for rows.Next() {
		var k string
		var last *time.Time
		var s RoutineState
		if err := rows.Scan(&k, &last, &s.Head); err != nil {
			return nil, err
		}
		if last != nil {
			s.Last = *last
		}
		out[k] = s
	}
	return out, rows.Err()
}

// DeleteRoutineState forgets key.
func (p *Postgres) DeleteRoutineState(ctx context.Context, key string) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	_, err := p.Pool.Exec(ctx, "DELETE FROM routine_state WHERE key = $1", key)
	return err
}

func (p *Postgres) SaveRoutineState(ctx context.Context, key string, s RoutineState) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	var last any
	if !s.Last.IsZero() {
		last = s.Last.UTC()
	}
	_, err := p.Pool.Exec(ctx, `INSERT INTO routine_state (key, last, head) VALUES ($1, $2, $3)
ON CONFLICT (key) DO UPDATE SET last = excluded.last, head = excluded.head`, key, last, s.Head)
	return err
}
