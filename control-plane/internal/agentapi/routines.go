package agentapi

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"homecloud/control-plane/internal/agents"
	"homecloud/control-plane/internal/gateway"
	"homecloud/control-plane/internal/sandbox"
)

// Routines runs the agents' routines: work an agent starts by itself, every
// so often or when a branch moves, for the routine's owner, with nobody
// watching. It acts with the agent's own account: it reads, and changes
// only what grants with unattended: true allow for the owner. Its answer is
// posted in the namespace's channel.
//
// When each last ran is kept in memory: after a restart, a routine runs
// again one interval later, and a push that came meanwhile is missed.
type Routines struct {
	Server *Server
	Gitea  *gateway.Gitea
	Tick   time.Duration // how often to look; 0: a minute
	Now    func() time.Time

	mu      sync.Mutex
	last    map[string]time.Time // routine -> when it last started
	heads   map[string]string    // routine -> the commit its branch was at
	running map[string]bool
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

// Check starts the routines that are due.
func (r *Routines) Check(ctx context.Context) {
	list, err := r.Server.Agents.All(ctx)
	if err != nil {
		log.Printf("routines: %v", err)
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.last == nil {
		r.last, r.heads, r.running = map[string]time.Time{}, map[string]string{}, map[string]bool{}
	}
	now := r.now()
	for _, a := range list {
		for _, rt := range a.Routines {
			key := a.ID() + "/" + rt.Name
			why := ""
			if rt.Every > 0 {
				last, seen := r.last[key]
				if !seen {
					r.last[key] = now // first seen: its first run is one interval on
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
				if !seen || head == prev || head == "" {
					continue
				}
				why = fmt.Sprintf("%s of %s/%s moved to %.12s", rt.OnPush.Branch, a.Namespace, rt.OnPush.Repo, head)
			}
			if r.running[key] {
				continue // still at it; the next one waits
			}
			r.last[key] = now
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
	// A private agent's findings go where only the namespace's people read.
	private, err := r.Gitea.AgentPrivate(ctx, &a)
	if err != nil {
		log.Printf("routines: %s: %v", key, err)
		return
	}
	post := func(text string) {
		if s.Poster == nil {
			return
		}
		if err := s.Poster.Post(ctx, gateway.Post{Agent: a.ID(), Team: private, Text: text}); err != nil {
			log.Printf("routines: %s: post: %v", key, err)
		}
	}
	role, err := r.Gitea.Role(ctx, a.Namespace, rt.Owner)
	if err != nil {
		log.Printf("routines: %s: %v", key, err)
		return
	}
	if role == "" {
		log.Printf("routines: %s: not run: its owner %s is not in %s", key, rt.Owner, a.Namespace)
		post(fmt.Sprintf("The routine %s did not run: its owner %s is not in %s. Change its owner in agents/%s/agent.yaml.", rt.Name, rt.Owner, a.Namespace, a.Name))
		return
	}
	log.Printf("routines: %s for %s: %s", key, rt.Owner, why)
	text := fmt.Sprintf(`This is your routine %q, for %s, started because %s. Nobody is watching: you act with your own account, and change only what your grants for runs without a person allow; otherwise you read and report. Your last message is posted in a channel of %s for its people to read, so say plainly what you found or did, with links.

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
	}
	post(fmt.Sprintf("Routine %s, for %s (%s):\n\n%s", rt.Name, rt.Owner, why, answer))
}
