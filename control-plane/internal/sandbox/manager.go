package sandbox

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net"
	"net/url"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Label marks the containers and volumes of sandboxes; the sandbox guard
// lets the agent service touch nothing else.
const Label = "garrison.sandbox"

// UnitLabel names the unit a sandbox works in; the guard binds
// only that unit's media library.
const UnitLabel = "garrison.unit"

// MediaTarget is where the unit's media library is in a sandbox.
const MediaTarget = "/data/media"

// Want is the sandbox a turn needs.
type Want struct {
	Key        string // one sandbox per key: a chat and an agent
	Chat       string
	Agent      string // <unit>/<agent>
	Person     string // whose message started the turn
	Model      string
	Prompt     string // the system prompt
	Definition string // changes when the agent's definition does
	// Media, if set, is the unit whose media library the sandbox mounts at
	// /data/media; a change restarts the sandbox between turns.
	Media string
	// Context, if set, is the model's context window in tokens, which Pi
	// keeps its conversation within (compacting it) and its answers to.
	Context int
	// Memory, if set, is the sandbox's memory in bytes instead of the
	// Manager's: an agent's agent.yaml may ask for more, e.g. designer, whose
	// ffmpeg cuts videos.
	Memory int64
}

// Unit is the unit the agent works in, from Agent.
func (w Want) Unit() string {
	ns, _, _ := strings.Cut(w.Agent, "/")
	return ns
}

// Manager starts, reuses and stops the sandboxes.
type Manager struct {
	Engine       Engine // Docker or Kubernetes
	Image        string
	Network      string
	API          string // the agent service, as sandboxes reach it
	Proxy        string // its proxy to the internet, as sandboxes reach it; empty: none
	Runtime      string // gVisor, where sandboxes are root: runsc (Docker) or the RuntimeClass gvisor (Kubernetes); empty: the default
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
	here     string // this service's address on Network, once found
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
	run       *Run      // whom the running turn is for; nil between turns
	since     time.Time // when the running turn started
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
	return "garrison-agent-" + hex.EncodeToString(sum[:10])
}

func (m *Manager) init() {
	if m.sessions == nil {
		m.sessions = map[string]*Session{}
		m.tokens = map[string]*Session{}
	}
}

// ErrBusy: the person has as many sandboxes working as allowed.
var ErrBusy = errors.New("busy")

// Run is whom a turn is for and what started it.
type Run struct {
	Token      string   // the Keycloak token of the person whose message started it; empty without one
	StartedBy  string   // person, agent <unit>/<agent>, routine <unit>/<agent>/<name>
	Chain      []string // the agents of this run, the first one first, this one last
	Unattended bool     // nobody is watching: a routine
	Owner      string   // then: the routine's owner
	Admin      bool     // the person is a root admin
	Task       string   // what the person asked, without the chat's context: the title of the turn's trace
	// Merge is the merge mode of the chain a routine runs (agents.Merge*),
	// which the gateway holds the run to; "" for other runs. Answered: the
	// run went on after its owner answered a question (ask_person).
	Merge    string
	Answered bool
	// Show, if set, shows progress to whoever waits for the run, e.g. the
	// person whose agent asked this one.
	Show func(string) `json:"-"`
	// Key is the sandbox's, set for the turn.
	Key string
	// Asked, if set, is set when the run asked a person (ask_person) and
	// waits for the answer. Thread, in a run that goes on after an answer,
	// is where it was asked: <channel>/<message>.
	Asked  *atomic.Bool `json:"-"`
	Thread string
	// Asks counts the agents this turn asked, set fresh for each turn.
	Asks *Asks `json:"-"`
}

// Asks counts how often a turn asked each agent, so that a lead asks one
// again, with a sharper task, only so often.
type Asks struct {
	mu   sync.Mutex
	n    map[string]int
	miss map[string]Miss
}

// Miss is an ask that did not finish: how far the agent got (partial or
// stuck), why, and the task it had.
type Miss struct {
	Status, Why, Task string
}

// Answered notes how agent's latest answer ended, a zero Miss when it was
// done, and says the miss before it, if there was one: so a done answer
// after a miss is a sharper ask that helped. A nil Asks notes nothing.
func (a *Asks) Answered(agent string, m Miss) (before Miss, ok bool) {
	if a == nil {
		return Miss{}, false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.miss == nil {
		a.miss = map[string]Miss{}
	}
	before, ok = a.miss[agent]
	if m.Status == "" {
		delete(a.miss, agent)
	} else {
		a.miss[agent] = m
	}
	return before, ok
}

// Add counts one more ask of agent and says how many there were, this one
// included. A nil Asks counts nothing and says 1.
func (a *Asks) Add(agent string) int {
	if a == nil {
		return 1
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.n == nil {
		a.n = map[string]int{}
	}
	a.n[agent]++
	return a.n[agent]
}

// Turn runs fn with the key's sandbox, started or restarted as needed, while
// the sandbox acts for run. Turns of one key run one after another; a
// sandbox whose agent definition or image has changed is restarted before
// the turn, never during one.
func (m *Manager) Turn(ctx context.Context, want Want, run Run, fn func(*Session) error) error {
	var s *Session
	for {
		m.mu.Lock()
		m.init()
		s = m.sessions[want.Key]
		if s == nil {
			s = &Session{name: Name(want.Key)}
			m.sessions[want.Key] = s
		}
		m.mu.Unlock()

		if err := lock(ctx, &s.turn); err != nil {
			return err
		}
		// Sweep may have forgotten the session while this waited for it:
		// then the turn takes the key's session of now.
		m.mu.Lock()
		current := m.sessions[want.Key] == s
		m.mu.Unlock()
		if current {
			break
		}
		s.turn.Unlock()
	}
	defer s.turn.Unlock()

	s.mu.Lock()
	stale := s.pi != nil && (s.want.Definition != want.Definition || s.want.Media != want.Media || s.want.Memory != want.Memory || s.want.Context != want.Context || s.image != m.Image)
	if s.pi != nil {
		select {
		case <-s.pi.Done():
			log.Printf("sandbox %s: Pi had stopped: %v; %s", s.name, s.pi.Err(), s.stderr)
			stale = true
		default:
		}
	}
	s.want = want
	run.Asks = &Asks{}
	s.run = &run
	s.since = time.Now()
	s.lastUsed = s.since
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.run = nil
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
	labels := map[string]string{Label: "true", "garrison.chat": want.Chat, "garrison.agent": want.Agent, "garrison.person": want.Person,
		UnitLabel: want.Unit()}
	// A container left from before (this service restarted) is replaced; the
	// volume, with the workspace and Pi's session, stays.
	if err := m.Engine.Remove(ctx, s.name); err != nil {
		return err
	}
	if err := m.Engine.CreateVolume(ctx, s.name, labels); err != nil {
		return err
	}
	api, proxy, err := m.addresses(ctx)
	if err != nil {
		return err
	}
	memory := m.Memory
	if want.Memory > 0 {
		memory = want.Memory
	}
	spec := Spec{
		Name: s.name, Image: m.Image, Labels: labels, Network: m.Network, Volume: s.name,
		Memory: memory, CPUs: m.CPUs, Pids: m.Pids, Runtime: m.Runtime, Media: want.Media,
		Env: []string{
			"GARRISON_API=" + api,
			"GARRISON_TOKEN=" + token,
			"GARRISON_MODEL=" + want.Model,
			"GARRISON_PROMPT=" + want.Prompt,
		},
	}
	if want.Context > 0 {
		// An answer may take an eighth of the window, at most 16384: Pi
		// compacts a conversation once it nears the window less its
		// reserve, and a turn's tool results still need room after that.
		spec.Env = append(spec.Env, fmt.Sprintf("GARRISON_CONTEXT=%d", want.Context), fmt.Sprintf("GARRISON_MAX_TOKENS=%d", min(want.Context/8, 16384)))
	}
	if proxy != "" {
		// The token names the sandbox to the proxy, as to the API: it counts
		// only during a turn. What it lets through is the agent's internet.
		u, err := url.Parse(proxy)
		if err != nil {
			return err
		}
		u.User = url.UserPassword("sandbox", token)
		apiURL, _ := url.Parse(api)
		for _, k := range []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy"} {
			spec.Env = append(spec.Env, k+"="+u.String())
		}
		for _, k := range []string{"NO_PROXY", "no_proxy"} {
			spec.Env = append(spec.Env, k+"="+apiURL.Hostname())
		}
	}
	id, err := m.Engine.Create(ctx, spec)
	if errors.Is(err, ErrNotFound) {
		if perr := m.Engine.Pull(ctx, m.Image, m.RegistryAuth); perr != nil {
			return perr
		}
		id, err = m.Engine.Create(ctx, spec)
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

	in, out, err := m.Engine.Attach(ctx, id, s.stderr)
	if err != nil {
		return err
	}
	pi := NewPi(in, out)
	if err := m.Engine.Start(ctx, id); err != nil {
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

// addresses are the API's and the proxy's, as sandboxes reach them. Under
// gVisor in Docker a sandbox has no Docker DNS, so names become this
// service's own address on the sandboxes' network. In Kubernetes (no
// Network) pods look the names up as any pod does.
func (m *Manager) addresses(ctx context.Context) (api, proxy string, err error) {
	if m.Runtime == "" || m.Network == "" {
		return m.API, m.Proxy, nil
	}
	m.mu.Lock()
	here := m.here
	m.mu.Unlock()
	if here == "" {
		subnets, err := m.Engine.Subnets(ctx, m.Network)
		if err != nil {
			return "", "", fmt.Errorf("the sandboxes' network: %w", err)
		}
		addrs, err := net.InterfaceAddrs()
		if err != nil {
			return "", "", err
		}
		here = OwnAddress(addrs, subnets)
		if here == "" {
			return "", "", fmt.Errorf("this service has no address on the network %s", m.Network)
		}
		m.mu.Lock()
		m.here = here
		m.mu.Unlock()
	}
	withHost := func(raw string) (string, error) {
		if raw == "" {
			return "", nil
		}
		u, err := url.Parse(raw)
		if err != nil {
			return "", err
		}
		port := u.Port()
		u.Host = here
		if port != "" {
			u.Host = net.JoinHostPort(here, port)
		}
		return u.String(), nil
	}
	if api, err = withHost(m.API); err != nil {
		return "", "", err
	}
	proxy, err = withHost(m.Proxy)
	return api, proxy, err
}

// OwnAddress is the first of addrs in one of subnets, or "".
func OwnAddress(addrs []net.Addr, subnets []*net.IPNet) string {
	for _, a := range addrs {
		ipn, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		for _, s := range subnets {
			if s.Contains(ipn.IP) {
				return ipn.IP.String()
			}
		}
	}
	return ""
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
	if err := m.Engine.Remove(ctx, id); err != nil {
		log.Printf("sandbox %s: %v", s.name, err)
	}
}

// Caller is who a sandbox acts for.
type Caller struct {
	Agent  string // <unit>/<agent>
	Model  string
	Chat   string
	Person string // whose turn runs: the person, or a routine's owner
	Token  string // their Keycloak token; empty without a person
	Run    Run
}

// Caller is who the sandbox with token acts for. ok is false for an unknown
// token, and between turns, when a sandbox may do nothing on the platform.
func (m *Manager) Caller(token string) (Caller, bool) {
	if token == "" {
		return Caller{}, false
	}
	m.mu.Lock()
	s := m.tokens[token]
	m.mu.Unlock()
	if s == nil {
		return Caller{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.run == nil {
		return Caller{}, false
	}
	return Caller{Agent: s.want.Agent, Model: s.want.Model, Chat: s.want.Chat, Person: s.want.Person, Token: s.run.Token, Run: *s.run}, true
}

// Running is a turn an agent is working on now, in its sandbox.
type Running struct {
	Unit      string    `json:"unit"`
	Agent     string    `json:"agent"`      // its name, as it works in Unit
	For       string    `json:"for"`        // the person it works for: whose message started it, or a routine's owner
	StartedBy string    `json:"started_by"` // person, agent <unit>/<agent>, routine <unit>/<agent>/<name>
	Chain     []string  `json:"chain"`      // the agents of the run, the first one first, this one last
	Since     time.Time `json:"since"`
}

// Turns lists the turns running now, the oldest first: the agents at work.
// A run that asked a person and waits for the answer has ended its turn, so
// it is not here; nor is a sandbox between turns (Running has those).
func (m *Manager) Turns() []Running {
	m.mu.Lock()
	all := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		all = append(all, s)
	}
	m.mu.Unlock()
	out := []Running{}
	for _, s := range all {
		s.mu.Lock()
		if s.run != nil {
			unit, agent, _ := strings.Cut(s.want.Agent, "/")
			who := s.want.Person
			if s.run.Unattended {
				who = s.run.Owner
			}
			out = append(out, Running{Unit: unit, Agent: agent, For: who, StartedBy: s.run.StartedBy,
				Chain: append([]string(nil), s.run.Chain...), Since: s.since})
		}
		s.mu.Unlock()
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Since.Before(out[j].Since) })
	return out
}

// Clean removes the sandbox containers left from before this service
// started: their turns ended with it.
func (m *Manager) Clean(ctx context.Context) error {
	list, err := m.Engine.List(ctx, Label)
	if err != nil {
		return err
	}
	for _, c := range list {
		if err := m.Engine.Remove(ctx, c.ID); err != nil {
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
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		m.Sweep(ctx)
	}
}

// Sweep stops the sandboxes without a message for Idle, and forgets the
// sessions without a sandbox: a chat's next message makes a new one, and
// its workspace is in its volume.
func (m *Manager) Sweep(ctx context.Context) {
	idle := m.Idle
	if idle <= 0 {
		idle = 15 * time.Minute
	}
	m.mu.Lock()
	all := make(map[string]*Session, len(m.sessions))
	for key, s := range m.sessions {
		all[key] = s
	}
	m.mu.Unlock()
	for key, s := range all {
		if !s.turn.TryLock() {
			continue // a turn runs
		}
		s.mu.Lock()
		running := s.pi != nil
		old := running && time.Since(s.lastUsed) > idle
		s.mu.Unlock()
		if old {
			log.Printf("sandbox %s: stopped after %s without a message", s.name, idle)
			m.stop(ctx, s)
		}
		if old || !running {
			// Under the session's turn lock: a turn waiting for it looks
			// again once it has the lock (Turn).
			m.mu.Lock()
			if m.sessions[key] == s {
				delete(m.sessions, key)
			}
			m.mu.Unlock()
		}
		s.turn.Unlock()
	}
}

// Sessions is how many chats the manager remembers, with a sandbox or not.
func (m *Manager) Sessions() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.sessions)
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
