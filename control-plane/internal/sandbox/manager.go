package sandbox

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"sort"
	"sync"
	"time"
)

// Label marks the containers and volumes of sandboxes; the sandbox guard
// lets the agent service touch nothing else.
const Label = "homecloud.sandbox"

// Want is the sandbox a turn needs.
type Want struct {
	Key        string // one sandbox per key: a chat and an agent
	Chat       string
	Agent      string // <namespace>/<agent>
	Person     string // whose message started the turn
	Model      string
	Prompt     string // the system prompt
	Definition string // changes when the agent's definition does
}

// Manager starts, reuses and stops the sandboxes.
type Manager struct {
	Docker       *Docker
	Image        string
	Network      string
	API          string // the agent service, as sandboxes reach it
	Memory       int64
	CPUs         float64
	Pids         int64
	Idle         time.Duration // a sandbox without a turn this long is stopped
	PerPerson    int           // running sandboxes per person
	RegistryAuth string        // X-Registry-Auth for pulling Image
	Ready        time.Duration // how long Pi may take to start

	mu       sync.Mutex
	sessions map[string]*Session // by key
	tokens   map[string]*Session
}

// Session is a chat's sandbox.
type Session struct {
	turn sync.Mutex // one turn at a time

	mu        sync.Mutex
	want      Want
	image     string
	name      string // of the container and its volume
	token     string
	container string
	pi        *Pi
	caller    string // the Keycloak token of the person whose turn runs
	lastUsed  time.Time
	stderr    *tail
}

// Pi is the session's harness.
func (s *Session) Pi() *Pi {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pi
}

// Name is the container's and the volume's name.
func (s *Session) Name() string { return s.name }

// Name of the container and volume of a key.
func Name(key string) string {
	sum := sha256.Sum256([]byte(key))
	return "homecloud-agent-" + hex.EncodeToString(sum[:10])
}

func (m *Manager) init() {
	if m.sessions == nil {
		m.sessions = map[string]*Session{}
		m.tokens = map[string]*Session{}
	}
}

// ErrBusy: the person has as many sandboxes working as allowed.
var ErrBusy = errors.New("busy")

// Turn runs fn with the key's sandbox, started or restarted as needed, while
// the sandbox acts for caller (the person's Keycloak token). Turns of one
// key run one after another; a sandbox whose agent definition or image has
// changed is restarted before the turn, never during one.
func (m *Manager) Turn(ctx context.Context, want Want, caller string, fn func(*Session) error) error {
	m.mu.Lock()
	m.init()
	s := m.sessions[want.Key]
	if s == nil {
		s = &Session{name: Name(want.Key)}
		m.sessions[want.Key] = s
	}
	m.mu.Unlock()

	if err := lock(ctx, &s.turn); err != nil {
		return err
	}
	defer s.turn.Unlock()

	s.mu.Lock()
	stale := s.pi != nil && (s.want.Definition != want.Definition || s.image != m.Image)
	if s.pi != nil {
		select {
		case <-s.pi.Done():
			log.Printf("sandbox %s: Pi had stopped: %v; %s", s.name, s.pi.Err(), s.stderr)
			stale = true
		default:
		}
	}
	s.want = want
	s.caller = caller
	s.lastUsed = time.Now()
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.caller = ""
		s.lastUsed = time.Now()
		s.mu.Unlock()
	}()

	if stale {
		log.Printf("sandbox %s: restarting for %s", s.name, want.Agent)
		m.stop(ctx, s)
	}
	if s.Pi() == nil {
		if err := m.makeRoom(ctx, want.Person, s); err != nil {
			return err
		}
		if err := m.start(ctx, s); err != nil {
			m.stop(context.Background(), s)
			return err
		}
	}
	return fn(s)
}

func lock(ctx context.Context, mu *sync.Mutex) error {
	for !mu.TryLock() {
		select {
		case <-ctx.Done():
			return fmt.Errorf("the agent is still busy with an earlier message: %w", ctx.Err())
		case <-time.After(200 * time.Millisecond):
		}
	}
	return nil
}

// makeRoom stops the person's oldest idle sandbox when they have as many
// running as allowed.
func (m *Manager) makeRoom(ctx context.Context, person string, except *Session) error {
	if m.PerPerson <= 0 {
		return nil
	}
	m.mu.Lock()
	var mine []*Session
	used := map[*Session]time.Time{}
	for _, s := range m.sessions {
		s.mu.Lock()
		if s != except && s.pi != nil && s.want.Person == person {
			mine = append(mine, s)
			used[s] = s.lastUsed
		}
		s.mu.Unlock()
	}
	m.mu.Unlock()
	if len(mine) < m.PerPerson {
		return nil
	}
	sort.Slice(mine, func(i, j int) bool { return used[mine[i]].Before(used[mine[j]]) })
	for _, s := range mine {
		if s.turn.TryLock() {
			log.Printf("sandbox %s: stopped to make room for %s", s.name, person)
			m.stop(ctx, s)
			s.turn.Unlock()
			return nil
		}
	}
	return fmt.Errorf("%w: %d of your agent chats are working; wait for one to finish", ErrBusy, len(mine))
}

func (m *Manager) start(ctx context.Context, s *Session) error {
	s.mu.Lock()
	want := s.want
	s.mu.Unlock()
	token, err := randomToken()
	if err != nil {
		return err
	}
	labels := map[string]string{Label: "true", "homecloud.chat": want.Chat, "homecloud.agent": want.Agent, "homecloud.person": want.Person}
	// A container left from before (this service restarted) is replaced; the
	// volume, with the workspace and Pi's session, stays.
	if err := m.Docker.Remove(ctx, s.name); err != nil {
		return err
	}
	if err := m.Docker.CreateVolume(ctx, s.name, labels); err != nil {
		return err
	}
	spec := Spec{
		Name: s.name, Image: m.Image, Labels: labels, Network: m.Network, Volume: s.name,
		Memory: m.Memory, CPUs: m.CPUs, Pids: m.Pids,
		Env: []string{
			"HOMECLOUD_API=" + m.API,
			"HOMECLOUD_TOKEN=" + token,
			"HOMECLOUD_MODEL=" + want.Model,
			"HOMECLOUD_PROMPT=" + want.Prompt,
		},
	}
	id, err := m.Docker.Create(ctx, spec)
	if errors.Is(err, ErrNotFound) {
		if perr := m.Docker.Pull(ctx, m.Image, m.RegistryAuth); perr != nil {
			return perr
		}
		id, err = m.Docker.Create(ctx, spec)
	}
	if err != nil {
		return err
	}
	// Known before Pi starts: its extension lists the tools as the caller.
	m.mu.Lock()
	s.mu.Lock()
	s.token, s.container, s.image = token, id, m.Image
	s.stderr = &tail{}
	s.mu.Unlock()
	m.tokens[token] = s
	m.mu.Unlock()

	in, out, err := m.Docker.Attach(ctx, id, s.stderr)
	if err != nil {
		return err
	}
	pi := NewPi(in, out)
	if err := m.Docker.Start(ctx, id); err != nil {
		pi.Close()
		return err
	}
	ready := m.Ready
	if ready <= 0 {
		ready = 2 * time.Minute
	}
	wait, cancel := context.WithTimeout(ctx, ready)
	defer cancel()
	if _, err := pi.State(wait); err != nil {
		pi.Close()
		return fmt.Errorf("the agent's sandbox did not start: %v %s", err, s.stderr)
	}
	s.mu.Lock()
	s.pi = pi
	s.mu.Unlock()
	log.Printf("sandbox %s: started for %s, chat %s, by %s", s.name, want.Agent, want.Chat, want.Person)
	return nil
}

// stop removes the session's container; its volume stays.
func (m *Manager) stop(ctx context.Context, s *Session) {
	s.mu.Lock()
	pi, id, token := s.pi, s.container, s.token
	s.pi, s.container, s.token = nil, "", ""
	s.mu.Unlock()
	if pi != nil {
		pi.Close()
	}
	m.mu.Lock()
	delete(m.tokens, token)
	m.mu.Unlock()
	if id == "" {
		id = s.name
	}
	if err := m.Docker.Remove(ctx, id); err != nil {
		log.Printf("sandbox %s: %v", s.name, err)
	}
}

// Caller is who a sandbox acts for: its agent, its model and the Keycloak
// token of the person whose turn runs. ok is false for an unknown token, and
// between turns, when a sandbox may do nothing on the platform.
func (m *Manager) Caller(token string) (agent, model, caller string, ok bool) {
	if token == "" {
		return "", "", "", false
	}
	m.mu.Lock()
	s := m.tokens[token]
	m.mu.Unlock()
	if s == nil {
		return "", "", "", false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.caller == "" {
		return "", "", "", false
	}
	return s.want.Agent, s.want.Model, s.caller, true
}

// Clean removes the sandbox containers left from before this service
// started: their turns ended with it.
func (m *Manager) Clean(ctx context.Context) error {
	list, err := m.Docker.List(ctx, Label)
	if err != nil {
		return err
	}
	for _, c := range list {
		if err := m.Docker.Remove(ctx, c.ID); err != nil {
			log.Printf("sandbox: %v", err)
		}
	}
	if len(list) > 0 {
		log.Printf("sandbox: removed %d containers of earlier runs", len(list))
	}
	return nil
}

// Run stops idle sandboxes until ctx ends.
func (m *Manager) Run(ctx context.Context) {
	idle := m.Idle
	if idle <= 0 {
		idle = 15 * time.Minute
	}
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		m.mu.Lock()
		var all []*Session
		for _, s := range m.sessions {
			all = append(all, s)
		}
		m.mu.Unlock()
		for _, s := range all {
			if !s.turn.TryLock() {
				continue // a turn runs
			}
			s.mu.Lock()
			old := s.pi != nil && time.Since(s.lastUsed) > idle
			s.mu.Unlock()
			if old {
				log.Printf("sandbox %s: stopped after %s without a message", s.name, idle)
				m.stop(ctx, s)
			}
			s.turn.Unlock()
		}
	}
}

// Running lists the running sandboxes.
func (m *Manager) Running() []Want {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Want
	for _, s := range m.sessions {
		s.mu.Lock()
		if s.pi != nil {
			out = append(out, s.want)
		}
		s.mu.Unlock()
	}
	return out
}

func randomToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// tail keeps the last few KB written to it: a sandbox's stderr.
type tail struct {
	mu  sync.Mutex
	buf []byte
}

func (t *tail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > 4096 {
		t.buf = t.buf[len(t.buf)-4096:]
	}
	return len(p), nil
}

func (t *tail) String() string {
	if t == nil {
		return ""
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.buf) == 0 {
		return ""
	}
	return "(stderr: " + string(t.buf) + ")"
}
