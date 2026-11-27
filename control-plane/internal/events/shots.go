package events

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

// Shots are screenshots an agent took to show in a chat, not to put into a
// repo: kept KeepShots, served by the dashboard at /shots/<id>.png to the
// person who asked, the unit's people and root admins.

// KeepShots is how long a screenshot is kept.
const KeepShots = 14 * 24 * time.Hour

// MaxShot is how large one may be.
const MaxShot = 8 << 20

// Shot is one screenshot.
type Shot struct {
	ID     string // random, hex
	Unit   string // whose app it shows, for who may see it
	Person string // who asked
	URL    string // the page
	At     time.Time
	PNG    []byte
}

// SeesShot tells whether v may see s.
func (v Visibility) SeesShot(s Shot) bool {
	return v.All || v.Person == s.Person || (s.Unit != "" && s.Unit != PlatformUnit && v.Members[s.Unit])
}

// Shots keeps screenshots.
type Shots interface {
	SaveShot(ctx context.Context, s Shot) (string, error) // its ID
	Shot(ctx context.Context, id string) (Shot, bool, error)
}

func newShotID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

const shotSchema = `
CREATE TABLE IF NOT EXISTS shots (
  id     text PRIMARY KEY,
  unit   text NOT NULL DEFAULT '',
  person text NOT NULL,
  url    text NOT NULL DEFAULT '',
  at     timestamptz NOT NULL,
  png    bytea NOT NULL
);
CREATE INDEX IF NOT EXISTS shots_by_at ON shots (at);
`

func (p *Postgres) SaveShot(ctx context.Context, s Shot) (string, error) {
	if len(s.PNG) == 0 || len(s.PNG) > MaxShot {
		return "", errors.New("a screenshot is empty or too large")
	}
	s.ID = newShotID()
	_, err := p.Pool.Exec(ctx, `INSERT INTO shots (id, unit, person, url, at, png) VALUES ($1, $2, $3, $4, $5, $6)`,
		s.ID, s.Unit, s.Person, clip(s.URL, 500), time.Now().UTC(), s.PNG)
	return s.ID, err
}

func (p *Postgres) Shot(ctx context.Context, id string) (Shot, bool, error) {
	var s Shot
	err := p.Pool.QueryRow(ctx, `SELECT id, unit, person, url, at, png FROM shots WHERE id = $1`, id).Scan(&s.ID, &s.Unit, &s.Person, &s.URL, &s.At, &s.PNG)
	if errors.Is(err, pgx.ErrNoRows) {
		return Shot{}, false, nil
	}
	return s, err == nil, err
}

// PruneShots drops screenshots older than KeepShots.
func (p *Postgres) PruneShots(ctx context.Context) error {
	_, err := p.Pool.Exec(ctx, `DELETE FROM shots WHERE at < $1`, time.Now().Add(-KeepShots))
	return err
}

func (l *Lazy) SaveShot(ctx context.Context, s Shot) (string, error) {
	pg, err := l.get(ctx)
	if err != nil {
		return "", err
	}
	return pg.SaveShot(ctx, s)
}

func (l *Lazy) Shot(ctx context.Context, id string) (Shot, bool, error) {
	pg, err := l.get(ctx)
	if err != nil {
		return Shot{}, false, err
	}
	return pg.Shot(ctx, id)
}

// MemoryShots keeps screenshots in memory, for tests.
type MemoryShots struct {
	mu    sync.Mutex
	shots map[string]Shot
}

func (m *MemoryShots) SaveShot(_ context.Context, s Shot) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.shots == nil {
		m.shots = map[string]Shot{}
	}
	s.ID, s.At = newShotID(), time.Now()
	m.shots[s.ID] = s
	return s.ID, nil
}

func (m *MemoryShots) Shot(_ context.Context, id string) (Shot, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.shots[id]
	return s, ok, nil
}
