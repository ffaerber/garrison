package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"slices"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Todo struct {
	ID        int64     `json:"id"`
	Title     string    `json:"title"`
	Done      bool      `json:"done"`
	CreatedBy string    `json:"created_by"`
	CreatedAt time.Time `json:"created_at"`
}

var ErrNotFound = errors.New("no such todo")

type Store interface {
	List(ctx context.Context) ([]Todo, error)
	Add(ctx context.Context, title, by string) (Todo, error)
	Toggle(ctx context.Context, id int64) error
	Delete(ctx context.Context, id int64) error
	Ping(ctx context.Context) error
}

// Postgres stores todos in a table it creates on start.
type Postgres struct{ pool *pgxpool.Pool }

// OpenPostgres connects, waiting up to two minutes for the database, which
// may start after the app.
func OpenPostgres(ctx context.Context, dsn string) (*Postgres, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(2 * time.Minute)
	for {
		if err = pool.Ping(ctx); err == nil {
			break
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("database did not answer: %w", err)
		}
		log.Printf("waiting for the database: %v", err)
		time.Sleep(3 * time.Second)
	}
	_, err = pool.Exec(ctx, `CREATE TABLE IF NOT EXISTS todos (
		id         bigserial PRIMARY KEY,
		title      text NOT NULL,
		done       boolean NOT NULL DEFAULT false,
		created_by text NOT NULL DEFAULT '',
		created_at timestamptz NOT NULL DEFAULT now()
	)`)
	if err != nil {
		return nil, err
	}
	return &Postgres{pool: pool}, nil
}

func (p *Postgres) List(ctx context.Context) ([]Todo, error) {
	rows, err := p.pool.Query(ctx, `SELECT id, title, done, created_by, created_at FROM todos ORDER BY done, id`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Todo, error) {
		var t Todo
		err := r.Scan(&t.ID, &t.Title, &t.Done, &t.CreatedBy, &t.CreatedAt)
		return t, err
	})
}

func (p *Postgres) Add(ctx context.Context, title, by string) (Todo, error) {
	t := Todo{Title: title, CreatedBy: by}
	err := p.pool.QueryRow(ctx, `INSERT INTO todos (title, created_by) VALUES ($1, $2) RETURNING id, created_at`, title, by).Scan(&t.ID, &t.CreatedAt)
	return t, err
}

func (p *Postgres) Toggle(ctx context.Context, id int64) error {
	tag, err := p.pool.Exec(ctx, `UPDATE todos SET done = NOT done WHERE id = $1`, id)
	if err == nil && tag.RowsAffected() == 0 {
		err = ErrNotFound
	}
	return err
}

func (p *Postgres) Delete(ctx context.Context, id int64) error {
	tag, err := p.pool.Exec(ctx, `DELETE FROM todos WHERE id = $1`, id)
	if err == nil && tag.RowsAffected() == 0 {
		err = ErrNotFound
	}
	return err
}

func (p *Postgres) Ping(ctx context.Context) error { return p.pool.Ping(ctx) }

// Memory is a Store for tests.
type Memory struct {
	mu    sync.Mutex
	next  int64
	todos []Todo
}

func (m *Memory) List(context.Context) ([]Todo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := slices.Clone(m.todos)
	slices.SortStableFunc(out, func(a, b Todo) int {
		if a.Done != b.Done {
			if a.Done {
				return 1
			}
			return -1
		}
		return int(a.ID - b.ID)
	})
	return out, nil
}

func (m *Memory) Add(_ context.Context, title, by string) (Todo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.next++
	t := Todo{ID: m.next, Title: title, CreatedBy: by, CreatedAt: time.Now()}
	m.todos = append(m.todos, t)
	return t, nil
}

func (m *Memory) Toggle(_ context.Context, id int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.todos {
		if m.todos[i].ID == id {
			m.todos[i].Done = !m.todos[i].Done
			return nil
		}
	}
	return ErrNotFound
}

func (m *Memory) Delete(_ context.Context, id int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.todos {
		if m.todos[i].ID == id {
			m.todos = slices.Delete(m.todos, i, i+1)
			return nil
		}
	}
	return ErrNotFound
}

func (m *Memory) Ping(context.Context) error { return nil }
