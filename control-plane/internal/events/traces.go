package events

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// A trace is what an agent did in one turn, step by step: its thinking,
// what it said, the tools it called and their results. The agent service
// records it as the turn runs; the dashboard shows it on the agent's page.
// A trace holds whatever the agent read for its person, private repos
// included, so it is shown like the answer it led to (TraceVisibility):
// a private chat's only to its person, a channel's or a routine's to the
// unit's people.

// Kinds of step.
const (
	StepThinking = "thinking"
	StepSays     = "says"
	StepTool     = "tool"   // a call: Tool, and its arguments in Text
	StepResult   = "result" // its result; Error if it failed
	StepError    = "error"  // the turn failed
)

// Step is one thing an agent did in a turn.
type Step struct {
	At    time.Time `json:"at"`
	Kind  string    `json:"kind"`
	Tool  string    `json:"tool,omitempty"`
	Text  string    `json:"text"`
	Error bool      `json:"error,omitempty"`
}

// Trace is one turn of an agent.
type Trace struct {
	ID        int64     `json:"id"`
	At        time.Time `json:"at"`
	Updated   time.Time `json:"updated"`
	Unit      string    `json:"unit"`
	Agent     string    `json:"agent"`  // unit/name, as it worked there
	Person    string    `json:"person"` // whom it worked for
	Chat      string    `json:"chat"`
	StartedBy string    `json:"started_by"`
	Shared    bool      `json:"shared"` // with the unit's people: a channel's or a routine's turn
	Status    string    `json:"status"` // running, ok, failed
	Task      string    `json:"task"`   // what it was asked, shortened
	Steps     []Step    `json:"steps,omitempty"`
	Calls     int       `json:"calls"` // tool calls
}

// Limits: a step's text and a trace's steps are cut, so one turn cannot
// fill the database.
const (
	MaxStepText   = 4000
	MaxTraceSteps = 400
	// KeepTraces is how long traces are kept.
	KeepTraces = 14 * 24 * time.Hour
)

// Shared tells whether a turn in chat is for the unit's people: one in its
// channel, or a routine's, whose answer goes there. Every other turn is a
// person's own.
func Shared(chat string) bool {
	return strings.HasPrefix(chat, "channel:") || strings.HasPrefix(chat, "routine:")
}

// TraceVisibility is who sees a trace: its person always; a shared one the
// people of its unit; root admins only garrison's shared ones, besides their
// own. Unlike the feed, root admins do not read everyone's turns.
func (v Visibility) SeesTrace(t Trace) bool {
	if v.Person != "" && t.Person == v.Person {
		return true
	}
	if !t.Shared {
		return false
	}
	return v.Members[t.Unit] || (v.All && t.Unit == PlatformUnit)
}

// PlatformUnit is garrison's own unit.
const PlatformUnit = "garrison"

// Tracer records traces: the agent service's side.
type Tracer interface {
	// Begin starts a trace; 0 if it cannot be recorded.
	Begin(ctx context.Context, t Trace) int64
	// Add appends steps to trace id.
	Add(ctx context.Context, id int64, steps ...Step)
	// End says how the turn ended.
	End(ctx context.Context, id int64, status string, calls int)
}

// TraceQuery lists traces.
type TraceQuery struct {
	Visibility Visibility
	Unit       string // only this unit's
	Agent      string // only this agent's (unit/name)
	Limit      int
}

const traceSchema = `
CREATE TABLE IF NOT EXISTS agent_traces (
  id         bigserial PRIMARY KEY,
  at         timestamptz NOT NULL,
  updated    timestamptz NOT NULL,
  unit       text NOT NULL,
  agent      text NOT NULL,
  person     text NOT NULL,
  chat       text NOT NULL DEFAULT '',
  started_by text NOT NULL DEFAULT '',
  shared     boolean NOT NULL DEFAULT false,
  status     text NOT NULL,
  task       text NOT NULL DEFAULT '',
  calls      integer NOT NULL DEFAULT 0,
  steps      jsonb NOT NULL DEFAULT '[]'
);
CREATE INDEX IF NOT EXISTS agent_traces_by_agent ON agent_traces (agent, id);
CREATE INDEX IF NOT EXISTS agent_traces_by_at ON agent_traces (at);
`

// clip cuts s to at most n bytes, on a rune, saying so.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8Start(s[n]) {
		n--
	}
	return s[:n] + " … (cut)"
}

func utf8Start(b byte) bool { return b&0xC0 != 0x80 }

func (p *Postgres) Begin(ctx context.Context, t Trace) int64 {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	now := time.Now().UTC()
	var id int64
	err := p.Pool.QueryRow(ctx, `
INSERT INTO agent_traces (at, updated, unit, agent, person, chat, started_by, shared, status, task)
VALUES ($1, $1, $2, $3, $4, $5, $6, $7, 'running', $8) RETURNING id`,
		now, t.Unit, t.Agent, t.Person, t.Chat, t.StartedBy, t.Shared, clip(t.Task, MaxStepText)).Scan(&id)
	if err != nil {
		return 0
	}
	return id
}

func (p *Postgres) Add(ctx context.Context, id int64, steps ...Step) {
	if id == 0 || len(steps) == 0 {
		return
	}
	for i := range steps {
		steps[i].Text = clip(steps[i].Text, MaxStepText)
		if steps[i].At.IsZero() {
			steps[i].At = time.Now().UTC()
		}
	}
	raw, err := json.Marshal(steps)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	// Past the limit, a trace keeps its first steps and says it was cut.
	p.Pool.Exec(ctx, `
UPDATE agent_traces SET updated = now(),
  steps = CASE WHEN jsonb_array_length(steps) < $2 THEN steps || $3::jsonb ELSE steps END
WHERE id = $1`, id, MaxTraceSteps, string(raw))
}

func (p *Postgres) End(ctx context.Context, id int64, status string, calls int) {
	if id == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	p.Pool.Exec(ctx, `UPDATE agent_traces SET updated = now(), status = $2, calls = $3 WHERE id = $1`, id, status, calls)
}

// traceVisible is the SQL condition for v on agent_traces, from $n on.
func traceVisible(v Visibility, n int) (string, []any) {
	return fmt.Sprintf(`(person = $%d OR (shared AND (unit = ANY($%d) OR ($%d AND unit = '%s'))))`, n, n+1, n+2, PlatformUnit),
		[]any{v.Person, keys(v.Members), v.All}
}

// Traces lists the traces v sees, newest first, without their steps.
func (p *Postgres) Traces(ctx context.Context, q TraceQuery) ([]Trace, error) {
	where, args := traceVisible(q.Visibility, 1)
	if q.Unit != "" {
		args = append(args, q.Unit)
		where += fmt.Sprintf(" AND unit = $%d", len(args))
	}
	if q.Agent != "" {
		args = append(args, q.Agent)
		where += fmt.Sprintf(" AND agent = $%d", len(args))
	}
	limit := q.Limit
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := p.Pool.Query(ctx, `
SELECT id, at, updated, unit, agent, person, chat, started_by, shared, status, task, calls
FROM agent_traces WHERE `+where+fmt.Sprintf(" ORDER BY id DESC LIMIT %d", limit), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Trace{}
	for rows.Next() {
		var t Trace
		if err := rows.Scan(&t.ID, &t.At, &t.Updated, &t.Unit, &t.Agent, &t.Person, &t.Chat, &t.StartedBy, &t.Shared, &t.Status, &t.Task, &t.Calls); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// Trace returns one trace with its steps, if v sees it.
func (p *Postgres) Trace(ctx context.Context, id int64, v Visibility) (Trace, bool, error) {
	where, args := traceVisible(v, 2)
	var t Trace
	var raw []byte
	err := p.Pool.QueryRow(ctx, `
SELECT id, at, updated, unit, agent, person, chat, started_by, shared, status, task, calls, steps
FROM agent_traces WHERE id = $1 AND `+where, append([]any{id}, args...)...).
		Scan(&t.ID, &t.At, &t.Updated, &t.Unit, &t.Agent, &t.Person, &t.Chat, &t.StartedBy, &t.Shared, &t.Status, &t.Task, &t.Calls, &raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return t, false, nil
	}
	if err != nil {
		return t, false, err
	}
	if err := json.Unmarshal(raw, &t.Steps); err != nil {
		return t, false, err
	}
	return t, true, nil
}

// PruneTraces deletes traces older than KeepTraces.
func (p *Postgres) PruneTraces(ctx context.Context) {
	p.Pool.Exec(ctx, `DELETE FROM agent_traces WHERE at < $1`, time.Now().Add(-KeepTraces))
}

// The traces through Lazy: recorded only while the database answers; a
// trace is a help to look into a turn, not a record to keep at any cost.

func (l *Lazy) Begin(ctx context.Context, t Trace) int64 {
	pg, err := l.get(context.WithoutCancel(ctx))
	if err != nil {
		return 0
	}
	return pg.Begin(ctx, t)
}

func (l *Lazy) Add(ctx context.Context, id int64, steps ...Step) {
	if id == 0 {
		return
	}
	if pg, err := l.get(context.WithoutCancel(ctx)); err == nil {
		pg.Add(ctx, id, steps...)
	}
}

func (l *Lazy) End(ctx context.Context, id int64, status string, calls int) {
	if id == 0 {
		return
	}
	if pg, err := l.get(context.WithoutCancel(ctx)); err == nil {
		pg.End(ctx, id, status, calls)
	}
}

func (l *Lazy) Traces(ctx context.Context, q TraceQuery) ([]Trace, error) {
	pg, err := l.get(ctx)
	if err != nil {
		return nil, err
	}
	return pg.Traces(ctx, q)
}

func (l *Lazy) Trace(ctx context.Context, id int64, v Visibility) (Trace, bool, error) {
	pg, err := l.get(ctx)
	if err != nil {
		return Trace{}, false, err
	}
	return pg.Trace(ctx, id, v)
}
