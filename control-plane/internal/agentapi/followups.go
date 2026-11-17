package agentapi

import (
	"context"
	"fmt"
	"log"
	"regexp"
	"strings"
	"time"

	"garrison/control-plane/internal/changes"
	"garrison/control-plane/internal/events"
	"garrison/control-plane/internal/gateway"
)

// Followups close the loop on the changes agents open. Once a person merges
// one, the agent that opened it says so in the namespace's channel, naming
// whom it was for, so they hear of it without looking; if merging deploys
// it, the agent says again once it is live, with its address, or that it
// failed. It follows the feed, from where it was when it started: what
// waits for a deploy is kept in memory, and lost with a restart.
type Followups struct {
	Feed  events.Store
	Cards interface {
		Card(ctx context.Context, person, ns, repo string, n int) (changes.Card, error)
	}
	Poster Poster
	Now    func() time.Time

	after   int64
	started bool
	waiting []awaited
}

// awaited is a merged change whose deploy is still to come.
type awaited struct {
	card   changes.Card
	agent  string // <namespace>/<agent>
	person string
	commit string   // an app's: the merge commit its deploy names
	stacks []string // the config's: the stacks it changes, <ns>/<stack>
	until  time.Time
	said   map[string]bool // the environments it was said of
}

// How long a merged change waits for its deploy: an app's image is built
// first.
const followupWait = 45 * time.Minute

var askedByNote = regexp.MustCompile(`(?m)^(?:Changes added, a|A)sked by ([A-Za-z0-9._-]+)(?: \(not in [a-z0-9-]+\))? through ([a-z0-9-]+/[a-z0-9-]+)[,.]`)

func (f *Followups) now() time.Time {
	if f.Now != nil {
		return f.Now()
	}
	return time.Now()
}

// Run follows the feed until ctx ends.
func (f *Followups) Run(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		f.Check(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Check reads what is new in the feed and says what is to be said.
func (f *Followups) Check(ctx context.Context) {
	all := events.Visibility{All: true}
	if !f.started {
		// From now on: what happened before is not news.
		es, err := f.Feed.List(ctx, events.Query{Visibility: all, Limit: 1})
		if err != nil {
			log.Printf("followups: %v", err)
			return
		}
		if len(es) > 0 {
			f.after = es[0].ID
		}
		f.started = true
		return
	}
	es, err := f.Feed.List(ctx, events.Query{Visibility: all, After: f.after, Kinds: []string{events.KindPRMerged, events.KindDeploy}, Limit: 200})
	if err != nil {
		log.Printf("followups: %v", err)
		return
	}
	// Oldest first.
	for i := len(es) - 1; i >= 0; i-- {
		e := es[i]
		if e.ID > f.after {
			f.after = e.ID
		}
		switch e.Kind {
		case events.KindPRMerged:
			f.merged(ctx, e)
		case events.KindDeploy:
			f.deployed(ctx, e)
		}
	}
	now := f.now()
	kept := f.waiting[:0]
	for _, w := range f.waiting {
		if now.Before(w.until) {
			kept = append(kept, w)
		} else if len(w.said) == 0 {
			log.Printf("followups: %s/%s#%d: no deploy within %s", w.card.Namespace, w.card.Repo, w.card.Number, followupWait)
		}
	}
	f.waiting = kept
}

func (f *Followups) post(ctx context.Context, agent, person, text string) {
	if _, err := f.Poster.Post(ctx, gateway.Post{Agent: agent, Mention: person, Text: text}); err != nil {
		log.Printf("followups: %s: post: %v", agent, err)
	}
}

func (f *Followups) merged(ctx context.Context, e events.Event) {
	if e.Namespace == "" || e.Repo == "" || e.PR == 0 {
		return
	}
	c, err := f.Cards.Card(ctx, "", e.Namespace, e.Repo, e.PR)
	if err != nil {
		log.Printf("followups: %s/%s#%d: %v", e.Namespace, e.Repo, e.PR, err)
		return
	}
	m := askedByNote.FindStringSubmatch(c.Body)
	if m == nil {
		return // no agent's
	}
	person, agent := m[1], m[2]
	w := awaited{card: c, agent: agent, person: person, until: f.now().Add(followupWait), said: map[string]bool{}}
	var then string
	switch {
	case c.Repo == gateway.ConfigRepo:
		for _, file := range c.Files {
			if name, ok := strings.CutPrefix(file.Path, "stacks/"); ok && strings.HasSuffix(name, ".yaml") && file.Status != "removed" && file.Status != "deleted" {
				w.stacks = append(w.stacks, c.Namespace+"/"+strings.TrimSuffix(name, ".yaml"))
			}
		}
		var parts []string
		if len(w.stacks) > 0 {
			parts = append(parts, "I will say here when it is deployed.")
		}
		for _, file := range c.Files {
			switch {
			case file.Path == "routines.yaml":
				parts = append(parts, "Its routines run from now on, as routines.yaml says; what they find is posted here.")
			case file.Path == "grants.yaml":
				parts = append(parts, "The agents have the rights it gives from now on.")
			case file.Path == "members.yaml":
				parts = append(parts, "Who belongs here is updated within a minute.")
			case strings.HasPrefix(file.Path, "restores/"):
				parts = append(parts, "The deployer brings the data back now; the app stops for it a moment.")
			}
		}
		then = strings.Join(parts, " ")
	case c.Base == "main" || c.Base == "dev":
		w.commit = e.Commit
		then = "Its pipeline builds it now; I will say here when it is deployed."
	}
	if !strings.HasPrefix(e.Actor, "agent-") {
		// An agent that merged it with its own account said so already.
		text := fmt.Sprintf("Your change [%s](%s) (%s/%s#%d) is merged, by %s.", c.Title, c.URL, c.Namespace, c.Repo, c.Number, e.Actor)
		if then != "" {
			text += " " + then
		}
		f.post(ctx, agent, person, text)
	}
	if w.commit != "" || len(w.stacks) > 0 {
		f.waiting = append(f.waiting, w)
	}
}

func (f *Followups) deployed(ctx context.Context, e events.Event) {
	if e.Status != events.StatusOK && e.Status != events.StatusFailed {
		return
	}
	for i := range f.waiting {
		w := &f.waiting[i]
		if e.Namespace != w.card.Namespace {
			continue
		}
		match := w.commit != "" && e.Repo == w.card.Repo && e.Commit == w.commit
		for _, s := range w.stacks {
			// Its title names the environment: "deployed todo/todo@main at …".
			match = match || strings.Contains(e.Title, " "+s+"@")
		}
		if !match || w.said[e.Title] {
			continue
		}
		w.said[e.Title] = true
		var text string
		if e.Status == events.StatusOK {
			text = fmt.Sprintf("Your change %s (%s/%s#%d) is live", w.card.Title, w.card.Namespace, w.card.Repo, w.card.Number)
			if e.Link != "" {
				text += ": " + e.Link
			}
			text += "."
		} else {
			text = fmt.Sprintf("Your change %s (%s/%s#%d) is merged, but deploying it failed: %s", w.card.Title, w.card.Namespace, w.card.Repo, w.card.Number, e.Title)
			if d := strings.TrimSpace(e.Detail); d != "" {
				text += "\n\n" + firstLine(d)
			}
			text += "\n\nAsk me to look into it."
		}
		f.post(ctx, w.agent, w.person, text)
	}
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	if len(line) > 300 {
		line = line[:300] + "…"
	}
	return line
}
