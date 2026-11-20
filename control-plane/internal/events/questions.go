package events

import (
	"context"
	"encoding/json"
	"slices"
	"sync"
	"time"
)

// Question is what an agent asked a person, in a run nobody watched: the
// run waits for the answer, then goes on where it stopped.
type Question struct {
	ID     int64
	At     time.Time
	Unit   string
	Agent  string // unit/name
	Person string // whom it asks
	Text   string
	// Where it was posted: an Open WebUI channel, the message, the thread it
	// is in (itself, unless asked in an earlier question's thread), and the
	// person's Open WebUI user, whose reply there after it is the answer.
	Channel, Message, Root, User string
	// The run to go on with: its sandbox key and chat, and the run itself.
	Key, Chat string
	Run       json.RawMessage
	// The feed's record of the question.
	EventKey string
	Status   string // QuestionWaiting, QuestionAnswered, QuestionExpired
	Answer   string
	Answered time.Time
}

const (
	QuestionWaiting  = "waiting"
	QuestionAnswered = "answered"
	QuestionExpired  = "expired"
)

// QuestionStore keeps the questions agents asked.
type QuestionStore interface {
	// AddQuestion keeps q and sets its ID.
	AddQuestion(ctx context.Context, q *Question) error
	// Questions are those with the status, oldest first.
	Questions(ctx context.Context, status string) ([]Question, error)
	// SetQuestion keeps a question's status, answer and when it was answered.
	SetQuestion(ctx context.Context, q Question) error
}

const questionSchema = `
CREATE TABLE IF NOT EXISTS questions (
  id        bigserial PRIMARY KEY,
  at        timestamptz NOT NULL,
  unit text NOT NULL,
  agent     text NOT NULL,
  person    text NOT NULL,
  text      text NOT NULL,
  channel   text NOT NULL,
  message   text NOT NULL,
  root      text NOT NULL,
  owui_user text NOT NULL,
  key       bytea NOT NULL, -- a sandbox key, which may hold NUL bytes
  chat      text NOT NULL,
  run       jsonb NOT NULL,
  event_key text NOT NULL,
  status    text NOT NULL,
  answer    text NOT NULL DEFAULT '',
  answered  timestamptz
);
CREATE INDEX IF NOT EXISTS questions_by_status ON questions (status, id);
`

func (p *Postgres) AddQuestion(ctx context.Context, q *Question) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	return p.Pool.QueryRow(ctx, `INSERT INTO questions
(at, unit, agent, person, text, channel, message, root, owui_user, key, chat, run, event_key, status)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14) RETURNING id`,
		q.At.UTC(), q.Unit, q.Agent, q.Person, q.Text, q.Channel, q.Message, q.Root, q.User, []byte(q.Key), q.Chat, []byte(q.Run), q.EventKey, q.Status).Scan(&q.ID)
}

func (p *Postgres) Questions(ctx context.Context, status string) ([]Question, error) {
	rows, err := p.Pool.Query(ctx, `SELECT id, at, unit, agent, person, text, channel, message, root, owui_user, key, chat, run, event_key, status, answer, answered
FROM questions WHERE status = $1 ORDER BY id`, status)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Question
	for rows.Next() {
		var q Question
		var run, key []byte
		var answered *time.Time
		if err := rows.Scan(&q.ID, &q.At, &q.Unit, &q.Agent, &q.Person, &q.Text, &q.Channel, &q.Message, &q.Root, &q.User,
			&key, &q.Chat, &run, &q.EventKey, &q.Status, &q.Answer, &answered); err != nil {
			return nil, err
		}
		q.Run, q.Key = run, string(key)
		if answered != nil {
			q.Answered = *answered
		}
		out = append(out, q)
	}
	return out, rows.Err()
}

func (p *Postgres) SetQuestion(ctx context.Context, q Question) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	var answered any
	if !q.Answered.IsZero() {
		answered = q.Answered.UTC()
	}
	_, err := p.Pool.Exec(ctx, "UPDATE questions SET status = $2, answer = $3, answered = $4 WHERE id = $1", q.ID, q.Status, q.Answer, answered)
	return err
}

// MemoryQuestions keeps questions in memory, for tests and for running
// without a database.
type MemoryQuestions struct {
	mu   sync.Mutex
	list []Question
}

func (m *MemoryQuestions) AddQuestion(_ context.Context, q *Question) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	q.ID = int64(len(m.list) + 1)
	m.list = append(m.list, *q)
	return nil
}

func (m *MemoryQuestions) Questions(_ context.Context, status string) ([]Question, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Question
	for _, q := range m.list {
		if q.Status == status {
			out = append(out, q)
		}
	}
	return out, nil
}

func (m *MemoryQuestions) SetQuestion(_ context.Context, q Question) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	i := slices.IndexFunc(m.list, func(x Question) bool { return x.ID == q.ID })
	if i >= 0 {
		m.list[i].Status, m.list[i].Answer, m.list[i].Answered = q.Status, q.Answer, q.Answered
	}
	return nil
}
