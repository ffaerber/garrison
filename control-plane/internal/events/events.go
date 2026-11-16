// Package events keeps the home cloud's feed: one record per thing that
// happened (a push, a pipeline run, a deploy, an agent's change, a routine's
// run) and how it went. The deployer, the gateway and the agent service
// record into one Postgres table; the dashboard shows it, to each person only
// what they may read.
package events

import (
	"context"
	"log"
	"strings"
	"time"
)

// Sources: who recorded an event.
const (
	SourceGitea    = "gitea"
	SourceDeployer = "deployer"
	SourceGateway  = "gateway"
	SourceAgents   = "agents"
	SourceAlerts   = "alerts" // Alertmanager
	SourceHome     = "home"   // the dashboard
)

// Statuses: how it went.
const (
	StatusInfo    = "info"    // it happened
	StatusOK      = "ok"      // it was done
	StatusRunning = "running" // it is being done; a later record with the same key says how it ended
	StatusWaiting = "waiting" // it waits for something, e.g. an image
	StatusFailed  = "failed"
	StatusRefused = "refused" // a rule said no
)

// AudienceMembers limits an event to the namespace's people, e.g. a
// routine's answer, which goes to its channel only they read. Without it,
// whoever may read the event's repo sees it.
const AudienceMembers = "members"

// Event is one entry of the feed.
type Event struct {
	ID      int64     `json:"id"`
	At      time.Time `json:"at"`
	Updated time.Time `json:"updated"`

	Source string `json:"source"`
	Kind   string `json:"kind"` // e.g. push, pipeline, deploy, pr-merged, propose_patch, routine
	Status string `json:"status"`

	// Where: a namespace and repo decide who sees it. An empty namespace is
	// the installation's own (system stacks), for everyone signed in; an
	// empty repo stands for the namespace's config.
	Namespace string `json:"namespace,omitempty"`
	Repo      string `json:"repo,omitempty"`
	Audience  string `json:"audience,omitempty"`

	Actor     string `json:"actor,omitempty"`      // the person it was for, or a Gitea account
	Agent     string `json:"agent,omitempty"`      // namespace/name, when an agent did it
	StartedBy string `json:"started_by,omitempty"` // person, agent <id>, routine <id>/<name>

	Title  string `json:"title"`
	Detail string `json:"detail,omitempty"`
	Link   string `json:"link,omitempty"`

	// What ties it to others: a commit and a pull request of its repo.
	Commit string `json:"commit,omitempty"`
	PR     int    `json:"pr,omitempty"`

	// Key makes recording idempotent: a second record with the same key
	// updates the first (its status, title, detail and link), e.g. a
	// redelivered webhook or a routine's run that ended.
	Key string `json:"key,omitempty"`
}

// Recorder takes events. Recording never fails the work it describes: a
// recorder logs what it could not keep.
type Recorder interface {
	Record(ctx context.Context, e Event)
}

// Discard records nothing, for where no store is configured.
type Discard struct{}

func (Discard) Record(context.Context, Event) {}

// Or returns r, or Discard if r is nil.
func Or(r Recorder) Recorder {
	if r == nil {
		return Discard{}
	}
	return r
}

// Limits on what is kept of a field; a feed shows the gist.
const (
	maxTitle  = 300
	maxDetail = 4000
)

// clean fills defaults and trims fields to their limits.
func (e Event) clean(now time.Time) Event {
	if e.At.IsZero() {
		e.At = now
	}
	e.At = e.At.UTC()
	e.Updated = now.UTC()
	if e.Status == "" {
		e.Status = StatusInfo
	}
	e.Title = cut(oneLine(e.Title), maxTitle)
	e.Detail = cut(strings.TrimSpace(e.Detail), maxDetail)
	if len(e.Commit) > 40 {
		e.Commit = e.Commit[:40]
	}
	return e
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

func cut(s string, n int) string {
	if len(s) <= n {
		return s
	}
	// Do not split a UTF-8 sequence.
	for n > 0 && n < len(s) && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n] + "…"
}

// Logged records into r and logs what it records, for code paths that only
// logged before.
func Logged(r Recorder) Recorder { return logged{Or(r)} }

type logged struct{ r Recorder }

func (l logged) Record(ctx context.Context, e Event) {
	log.Printf("event: %s %s %s/%s: %s", e.Kind, e.Status, e.Namespace, e.Repo, e.Title)
	l.r.Record(ctx, e)
}
