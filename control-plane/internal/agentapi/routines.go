package agentapi

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"homecloud/control-plane/internal/agents"
	"homecloud/control-plane/internal/events"
	"homecloud/control-plane/internal/gateway"
	"homecloud/control-plane/internal/sandbox"
)

// Routines runs the agents' routines: work an agent starts by itself, every
// so often or when a branch moves, for the routine's owner, with nobody
// watching. It acts with the agent's own account: it reads, and changes
// only what grants with unattended: true allow for the owner. Its answer is
// posted in the namespace's channel.
//
// When each last ran, and the commit each on_push routine last saw, are
// kept in Store: after a restart, a schedule missed meanwhile runs once, and
// a push that came meanwhile starts its routine. Without a Store, they are
// kept in memory only.
type Routines struct {
	Server   *Server
	Gitea    *gateway.Gitea
	Store    RoutineStore   // nil: memory only
	Location *time.Location // the time zone schedules are read in; nil: UTC
	Tick     time.Duration  // how often to look; 0: a minute
	Now      func() time.Time

	mu      sync.Mutex
	loaded  bool
	last    map[string]time.Time // routine -> when it last started
	heads   map[string]string    // routine -> the commit its branch was at
	running map[string]bool
}

// RoutineState is what the scheduler keeps of a routine.
type RoutineState = events.RoutineState

// RoutineStore keeps the routines' state across restarts.
type RoutineStore interface {
	RoutineStates(ctx context.Context) (map[string]RoutineState, error)
	SaveRoutineState(ctx context.Context, key string, s RoutineState) error
}

func (r *Routines) loc() *time.Location {
	if r.Location != nil {
		return r.Location
	}
	return time.UTC
}

// save keeps key's state, if there is a store.
func (r *Routines) save(ctx context.Context, key string) {
	if r.Store == nil {
		return
	}
	if err := r.Store.SaveRoutineState(ctx, key, RoutineState{Last: r.last[key], Head: r.heads[key]}); err != nil {
		log.Printf("routines: %s: keep its state: %v", key, err)
	}
}

func (r *Routines) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// Run looks for due routines until ctx ends.
func (r *Routines) Run(ctx context.Context) {
	tick := r.Tick
	if tick == 0 {
		tick = time.Minute
	}
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		r.Check(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// prepare readies the state, loading it from the store once. Without what
// ran before, nothing runs: a schedule could run twice, or a push go
// unnoticed. Called with r.mu held.
func (r *Routines) prepare(ctx context.Context) bool {
	if r.last == nil {
		r.last, r.heads, r.running = map[string]time.Time{}, map[string]string{}, map[string]bool{}
	}
	if r.Store != nil && !r.loaded {
		states, err := r.Store.RoutineStates(ctx)
		if err != nil {
			log.Printf("routines: waiting for their state: %v", err)
			return false
		}
		for k, s := range states {
			if !s.Last.IsZero() {
				r.last[k] = s.Last
			}
			if s.Head != "" {
				r.heads[k] = s.Head
			}
		}
		r.loaded = true
	}
	return true
}

// start runs a routine in the background, unless it is running. Called
// with r.mu held.
func (r *Routines) start(ctx context.Context, a agents.Agent, rt agents.Routine, key, why string) bool {
	if r.running[key] {
		return false
	}
	r.running[key] = true
	go func() {
		defer func() {
			r.mu.Lock()
			r.running[key] = false
			r.mu.Unlock()
		}()
		r.run(context.WithoutCancel(ctx), a, rt, key, why)
	}()
	return true
}

// Check starts the routines that are due.
func (r *Routines) Check(ctx context.Context) {
	list, err := r.Server.Agents.All(ctx)
	if err != nil {
		log.Printf("routines: %v", err)
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.prepare(ctx) {
		return
	}
	now := r.now()
	for _, a := range list {
		for _, rt := range a.Routines {
			key := a.ID() + "/" + rt.Name
			why := ""
			if rt.Cron != nil {
				last, seen := r.last[key]
				if !seen {
					r.last[key] = now // first seen: it runs at the next time its schedule names
					r.save(ctx, key)
					continue
				}
				next := rt.Cron.Next(last, r.loc())
				if next.IsZero() || next.After(now) {
					continue
				}
				why = fmt.Sprintf("it is scheduled for %s (%s, %s)", next.Format("Mon 2 Jan 15:04"), rt.Schedule, r.loc())
			} else if rt.Every > 0 {
				last, seen := r.last[key]
				if !seen {
					r.last[key] = now // first seen: its first run is one interval on
					r.save(ctx, key)
					continue
				}
				if now.Sub(last) < rt.Every {
					continue
				}
				why = "it runs every " + rt.Raw
			} else if rt.OnPush != nil {
				head, err := r.Gitea.BranchHead(ctx, a.Namespace, rt.OnPush.Repo, rt.OnPush.Branch)
				if err != nil {
					log.Printf("routines: %s: %v", key, err)
					continue
				}
				prev, seen := r.heads[key]
				r.heads[key] = head
				if head != prev {
					r.save(ctx, key)
				}
				if !seen || head == prev || head == "" {
					continue
				}
				why = fmt.Sprintf("%s of %s/%s moved to %.12s", rt.OnPush.Branch, a.Namespace, rt.OnPush.Repo, head)
			} else {
				continue // on_alert: started by alerts, not by the clock
			}
			if r.running[key] {
				continue // still at it; the next one waits
			}
			r.last[key] = now
			r.save(ctx, key)
			r.running[key] = true
			go func(a agents.Agent, rt agents.Routine, key, why string) {
				defer func() {
					r.mu.Lock()
					r.running[key] = false
					r.mu.Unlock()
				}()
				r.run(ctx, a, rt, key, why)
			}(a, rt, key, why)
		}
	}
}

func (r *Routines) run(ctx context.Context, a agents.Agent, rt agents.Routine, key, why string) {
	s := r.Server
	// Into the namespace's channel, which only its people read.
	post := func(text string) {
		if s.Poster == nil {
			return
		}
		if err := s.Poster.Post(ctx, gateway.Post{Agent: a.ID(), Text: text}); err != nil {
			log.Printf("routines: %s: post: %v", key, err)
		}
	}
	// The feed gets the run, for the namespace's people, as its channel.
	ev := events.Event{Source: events.SourceAgents, Kind: events.KindRoutine, Status: events.StatusRunning,
		Namespace: a.Namespace, Audience: events.AudienceMembers, Actor: rt.Owner, Agent: a.ID(), StartedBy: "routine " + key,
		Title: fmt.Sprintf("routine %s of %s, for %s: %s", rt.Name, a.ID(), rt.Owner, why), Key: fmt.Sprintf("routine:%s:%d", key, r.now().UnixNano())}
	record := func(status, detail string) {
		if s.Events != nil {
			ev.Status, ev.Detail = status, detail
			s.Events.Record(ctx, ev)
		}
	}
	role, err := r.Gitea.Role(ctx, a.Namespace, rt.Owner)
	if err != nil {
		log.Printf("routines: %s: %v", key, err)
		record(events.StatusFailed, err.Error())
		return
	}
	if role == "" {
		log.Printf("routines: %s: not run: its owner %s is not in %s", key, rt.Owner, a.Namespace)
		record(events.StatusRefused, fmt.Sprintf("Not run: its owner %s is not in %s.", rt.Owner, a.Namespace))
		post(fmt.Sprintf("The routine %s did not run: its owner %s is not in %s. Change its owner in agents/%s/agent.yaml.", rt.Name, rt.Owner, a.Namespace, a.Name))
		return
	}
	log.Printf("routines: %s for %s: %s", key, rt.Owner, why)
	record(events.StatusRunning, "")
	text := fmt.Sprintf(`This is your routine %q, for %s, started because %s. Nobody is watching: you act with your own account, and change only what your grants for runs without a person allow; otherwise you read and report. Your last message is posted in the channel of %s for its people to read, so say plainly what you found or did, with links.

Task: %s`, rt.Name, rt.Owner, why, a.Namespace, strings.TrimSpace(rt.Task))
	msg := func(bool) (string, []sandbox.Image) { return text, nil }
	run := sandbox.Run{StartedBy: "routine " + key, Chain: []string{a.ID()}, Unattended: true, Owner: rt.Owner}
	tctx, cancel := context.WithTimeout(ctx, s.turnTimeout())
	defer cancel()
	out := &collect{}
	_, err = s.sandboxTurn(tctx, run, rt.Owner, "routine:"+key, "routine\x00"+key, a, msg, out)
	answer := out.answer()
	if err != nil {
		log.Printf("routines: %s: %v", key, err)
		answer = strings.TrimSpace(answer + "\n\nIt stopped: " + err.Error())
		record(events.StatusFailed, answer)
	} else {
		record(events.StatusOK, answer)
	}
	post(fmt.Sprintf("Routine %s, for %s (%s):\n\n%s", rt.Name, rt.Owner, why, answer))
}
