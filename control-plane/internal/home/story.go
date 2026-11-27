package home

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"garrison/control-plane/internal/events"
)

// story is what belongs to an event, newest first: the entries that
// share its commit, its pull request or its agent's run, as Related finds
// them, with what ties them together said in words, and the same entry
// repeated in a row (the same commit deployed again after a restart)
// folded into one with a count.
type story struct {
	About string // what ties them, e.g. "commit 22bb220 of garrison/sso"
	// Repo is the repo the event's commit is in, <unit>/<repo>, for its link
	// to Gitea; "" if it cannot be told.
	Repo  string
	Why   string // a sentence on what that means
	Steps []storyStep
	Only  bool // nothing else belongs to it
}

type storyStep struct {
	E     events.Event
	Times int         // how often in a row; 1 once
	At    []time.Time // when, each time
	This  bool        // the event the page is about
}

func shortCommit(c string) string {
	if len(c) > 7 {
		return c[:7]
	}
	return c
}

func repoName(e events.Event) string {
	if e.Repo == "" {
		return e.Unit
	}
	return e.Unit + "/" + e.Repo
}

func storyOf(e events.Event, related []events.Event) story {
	var s story
	switch {
	case e.PR != 0:
		s.About = fmt.Sprintf("pull request #%d of %s", e.PR, repoName(e))
		s.Why = "Everything that happened to this change, from proposing it to deploying it: its commits, reviews, checks, merge and deploys."
	case e.Commit != "":
		s.About = fmt.Sprintf("commit %s of %s", shortCommit(e.Commit), orUnit(commitRepo(e, related), e))
		s.Why = "Everything that happened to this version: how it came in (push or merge), its pipeline, and each time it was deployed."
	case (e.Kind == events.KindRoutine || e.Kind == events.KindAsk) && e.StartedBy != "":
		s.About = "this run"
		s.Why = "What was done for it while it ran: the changes proposed, the agents asked, what they made."
	default:
		s.About = "this entry"
		s.Why = "No commit, pull request or run ties it to other entries."
	}
	s.Repo = commitRepo(e, related)
	list := slices.Clone(related)
	if len(list) == 0 {
		list = []events.Event{e}
	}
	slices.SortStableFunc(list, func(a, b events.Event) int { return b.At.Compare(a.At) })
	for _, x := range list {
		if n := len(s.Steps); n > 0 {
			last := &s.Steps[n-1]
			if last.E.Kind == x.Kind && last.E.Title == x.Title && last.E.Status == x.Status {
				last.Times++
				last.At = append(last.At, x.At)
				last.This = last.This || x.ID == e.ID
				continue
			}
		}
		s.Steps = append(s.Steps, storyStep{E: x, Times: 1, At: []time.Time{x.At}, This: x.ID == e.ID})
	}
	s.Only = len(s.Steps) == 1
	return s
}

var systemStack = regexp.MustCompile(`the system stack ([a-z0-9-]+)\b`)

// commitRepo is the repo an event's commit is in, <unit>/<repo>: its own
// repo; for a system stack's deploy, which names none, the stack's repo of
// garrison; for a restore, or a failure of one, its unit's config; else the
// repo of a push or merge of the same commit.
func commitRepo(e events.Event, related []events.Event) string {
	if e.Commit == "" {
		return ""
	}
	unit := e.Unit
	if unit == "" {
		unit = events.PlatformUnit
	}
	switch {
	case e.Repo != "":
		return unit + "/" + e.Repo
	case e.Unit == "" && systemStack.MatchString(e.Title):
		return events.PlatformUnit + "/" + systemStack.FindStringSubmatch(e.Title)[1]
	case e.Kind == events.KindRestore || strings.HasPrefix(e.Title, e.Unit+"/config/"):
		return unit + "/config"
	}
	for _, x := range related {
		if x.Commit == e.Commit && x.Unit == e.Unit && x.Repo != "" {
			return unit + "/" + x.Repo
		}
	}
	return ""
}

// orUnit is repo, or the event's unit when it is not known.
func orUnit(repo string, e events.Event) string {
	if repo != "" {
		return repo
	}
	return repoName(e)
}
