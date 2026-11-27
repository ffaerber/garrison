package events

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

// Invites are one-time links for people, made on the dashboard's People
// page and sent by hand (Garrison sends no mail): an invite makes a new
// person, into a unit with a role if it names one; a reset sets a person's
// password. Only a hash of each link's token is kept.

// Invite kinds.
const (
	InviteNew   = "invite"
	InviteReset = "reset"
)

// KeepInvites is how long a link works.
const KeepInvites = 7 * 24 * time.Hour

// Invite is one link.
type Invite struct {
	ID       int64
	Kind     string    // InviteNew or InviteReset
	Unit     string    // the unit an invite adds to; "" for none
	Role     string    // admin or member, with Unit
	Username string    // a reset's person; an invite's, once used
	Note     string    // whom it is for, in the inviter's words
	By       string    // who made it
	At       time.Time // when
	Expires  time.Time
	Used     time.Time // zero while open
}

// Open tells whether the link still works at now.
func (i Invite) Open(now time.Time) bool { return i.Used.IsZero() && now.Before(i.Expires) }

// Invites keeps the links.
type Invites interface {
	// MakeInvite keeps in, found by hash, and returns its ID.
	MakeInvite(ctx context.Context, in Invite, hash string) (int64, error)
	// OpenInvites are the links still open: all of them, or those made for
	// units, and those by the person by.
	OpenInvites(ctx context.Context, all bool, units []string, by string) ([]Invite, error)
	// InviteByHash is the open link of hash.
	InviteByHash(ctx context.Context, hash string) (Invite, bool, error)
	// UseInvite marks an open link used by username; false if it was not
	// open any more (used, revoked or expired), as by a second click.
	UseInvite(ctx context.Context, id int64, username string) (bool, error)
	// ReleaseInvite opens a link again that UseInvite took, when what it
	// was for failed (a username that was taken).
	ReleaseInvite(ctx context.Context, id int64) error
	// RevokeInvite ends a link.
	RevokeInvite(ctx context.Context, id int64) error
}

const inviteSchema = `
CREATE TABLE IF NOT EXISTS invites (
  id       bigserial PRIMARY KEY,
  hash     text NOT NULL UNIQUE,
  kind     text NOT NULL,
  unit     text NOT NULL DEFAULT '',
  role     text NOT NULL DEFAULT '',
  username text NOT NULL DEFAULT '',
  note     text NOT NULL DEFAULT '',
  made_by  text NOT NULL,
  at       timestamptz NOT NULL,
  expires  timestamptz NOT NULL,
  used     timestamptz
);
`

const inviteColumns = `id, kind, unit, role, username, note, made_by, at, expires, used`

func scanInvite(row pgx.Row) (Invite, error) {
	var i Invite
	var used *time.Time
	err := row.Scan(&i.ID, &i.Kind, &i.Unit, &i.Role, &i.Username, &i.Note, &i.By, &i.At, &i.Expires, &used)
	if used != nil {
		i.Used = *used
	}
	return i, err
}

func (p *Postgres) MakeInvite(ctx context.Context, in Invite, hash string) (int64, error) {
	var id int64
	err := p.Pool.QueryRow(ctx, `
INSERT INTO invites (hash, kind, unit, role, username, note, made_by, at, expires)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) RETURNING id`,
		hash, in.Kind, in.Unit, in.Role, in.Username, clip(in.Note, 200), in.By, in.At.UTC(), in.Expires.UTC()).Scan(&id)
	return id, err
}

func (p *Postgres) OpenInvites(ctx context.Context, all bool, units []string, by string) ([]Invite, error) {
	if units == nil {
		units = []string{}
	}
	rows, err := p.Pool.Query(ctx, `SELECT `+inviteColumns+` FROM invites
WHERE used IS NULL AND expires > now() AND ($1 OR (unit <> '' AND unit = ANY($2)) OR made_by = $3)
ORDER BY id DESC LIMIT 200`, all, units, by)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Invite
	for rows.Next() {
		i, err := scanInvite(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, i)
	}
	return out, rows.Err()
}

func (p *Postgres) InviteByHash(ctx context.Context, hash string) (Invite, bool, error) {
	i, err := scanInvite(p.Pool.QueryRow(ctx, `SELECT `+inviteColumns+` FROM invites WHERE hash = $1 AND used IS NULL AND expires > now()`, hash))
	if errors.Is(err, pgx.ErrNoRows) {
		return Invite{}, false, nil
	}
	return i, err == nil, err
}

func (p *Postgres) UseInvite(ctx context.Context, id int64, username string) (bool, error) {
	tag, err := p.Pool.Exec(ctx, `UPDATE invites SET used = now(), username = CASE WHEN kind = 'invite' THEN $2 ELSE username END
WHERE id = $1 AND used IS NULL AND expires > now()`, id, username)
	return err == nil && tag.RowsAffected() == 1, err
}

func (p *Postgres) ReleaseInvite(ctx context.Context, id int64) error {
	_, err := p.Pool.Exec(ctx, `UPDATE invites SET used = NULL, username = CASE WHEN kind = 'invite' THEN '' ELSE username END WHERE id = $1`, id)
	return err
}

// RevokeInvite ends a link by making it expire now; it stays recorded.
func (p *Postgres) RevokeInvite(ctx context.Context, id int64) error {
	_, err := p.Pool.Exec(ctx, `UPDATE invites SET expires = now() WHERE id = $1 AND used IS NULL`, id)
	return err
}

// PruneInvites drops links that ended more than KeepInvites ago.
func (p *Postgres) PruneInvites(ctx context.Context) error {
	_, err := p.Pool.Exec(ctx, `DELETE FROM invites WHERE expires < $1`, time.Now().Add(-KeepInvites))
	return err
}

func (l *Lazy) MakeInvite(ctx context.Context, in Invite, hash string) (int64, error) {
	pg, err := l.get(ctx)
	if err != nil {
		return 0, err
	}
	return pg.MakeInvite(ctx, in, hash)
}

func (l *Lazy) OpenInvites(ctx context.Context, all bool, units []string, by string) ([]Invite, error) {
	pg, err := l.get(ctx)
	if err != nil {
		return nil, err
	}
	return pg.OpenInvites(ctx, all, units, by)
}

func (l *Lazy) InviteByHash(ctx context.Context, hash string) (Invite, bool, error) {
	pg, err := l.get(ctx)
	if err != nil {
		return Invite{}, false, err
	}
	return pg.InviteByHash(ctx, hash)
}

func (l *Lazy) UseInvite(ctx context.Context, id int64, username string) (bool, error) {
	pg, err := l.get(ctx)
	if err != nil {
		return false, err
	}
	return pg.UseInvite(ctx, id, username)
}

func (l *Lazy) ReleaseInvite(ctx context.Context, id int64) error {
	pg, err := l.get(ctx)
	if err != nil {
		return err
	}
	return pg.ReleaseInvite(ctx, id)
}

func (l *Lazy) RevokeInvite(ctx context.Context, id int64) error {
	pg, err := l.get(ctx)
	if err != nil {
		return err
	}
	return pg.RevokeInvite(ctx, id)
}

// MemoryInvites keeps links in memory, for tests.
type MemoryInvites struct {
	mu   sync.Mutex
	list []Invite
	hash map[int64]string
	Now  func() time.Time
}

func (m *MemoryInvites) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

func (m *MemoryInvites) MakeInvite(_ context.Context, in Invite, hash string) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.hash == nil {
		m.hash = map[int64]string{}
	}
	in.ID = int64(len(m.list) + 1)
	m.list = append(m.list, in)
	m.hash[in.ID] = hash
	return in.ID, nil
}

func (m *MemoryInvites) OpenInvites(_ context.Context, all bool, units []string, by string) ([]Invite, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Invite
	for i := len(m.list) - 1; i >= 0; i-- {
		in := m.list[i]
		mine := all || in.By == by
		for _, u := range units {
			mine = mine || (in.Unit != "" && in.Unit == u)
		}
		if in.Open(m.now()) && mine {
			out = append(out, in)
		}
	}
	return out, nil
}

func (m *MemoryInvites) InviteByHash(_ context.Context, hash string) (Invite, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, in := range m.list {
		if m.hash[in.ID] == hash && in.Open(m.now()) {
			return in, true, nil
		}
	}
	return Invite{}, false, nil
}

func (m *MemoryInvites) UseInvite(_ context.Context, id int64, username string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.list {
		if m.list[i].ID == id && m.list[i].Open(m.now()) {
			m.list[i].Used = m.now()
			if m.list[i].Kind == InviteNew {
				m.list[i].Username = username
			}
			return true, nil
		}
	}
	return false, nil
}

func (m *MemoryInvites) ReleaseInvite(_ context.Context, id int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.list {
		if m.list[i].ID == id {
			m.list[i].Used = time.Time{}
			if m.list[i].Kind == InviteNew {
				m.list[i].Username = ""
			}
		}
	}
	return nil
}

func (m *MemoryInvites) RevokeInvite(_ context.Context, id int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.list {
		if m.list[i].ID == id && m.list[i].Used.IsZero() {
			m.list[i].Expires = m.now()
		}
	}
	return nil
}
