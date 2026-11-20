// Package changes describes a proposed change (a pull request in Gitea) in
// plain words, for people who would rather not read a diff: what it
// touches, who proposed it, what it still waits for and what they can do
// about it. The dashboard shows these as cards; agents link to them.
package changes

import (
	"errors"
	"fmt"
	"path"
	"strings"
	"time"
)

// Path is the dashboard's page of a pull request: its card.
func Path(ns, repo string, n int) string { return fmt.Sprintf("/c/%s/%s/%d", ns, repo, n) }

// ErrForbidden is Gitea refusing the person.
var ErrForbidden = errors.New("not allowed")

// Card is one pull request, as a person sees it.
type Card struct {
	Unit      string
	Repo      string
	Number    int
	Title     string
	Body      string
	Author    string // who it is proposed as: the person an agent worked for, or an agent's own account
	Base      string // the branch it goes into
	URL       string // the pull request in Gitea
	Created   time.Time
	Files     []File
	Approvals []string // who approved it, as it is now
	Checks    []Check
	Merged    bool
	Closed    bool
	// NoApproval: the branch it goes into asks for no approval, as main of a
	// unit of one person, or a branch without a rule.
	NoApproval bool

	// For the person looking at it.
	Mine     bool // they proposed it
	Approved bool // they approved it
}

// File is one file the change touches.
type File struct {
	Path   string
	Status string // added, modified, removed, renamed
}

// Check is one status check on the change's head commit.
type Check struct {
	Name   string
	State  string // pending, success, error, failure, warning
	Detail string
}

// What says in plain words what a file of a repo is for.
func (f File) What(repo string) string {
	p := f.Path
	base := path.Base(p)
	if repo == "config" {
		switch {
		case p == "routines.yaml":
			return "what agents do on their own, on a schedule or when something happens (routines)"
		case p == "grants.yaml":
			return "what agents may do here (grants)"
		case p == "members.yaml":
			return "who belongs here, and who is an admin"
		case strings.HasPrefix(p, "stacks/") && strings.HasSuffix(p, ".yaml"):
			return fmt.Sprintf("how the app %s is deployed", strings.TrimSuffix(strings.TrimPrefix(p, "stacks/"), ".yaml"))
		case strings.HasPrefix(p, "restores/"):
			return "brings an app's data back from a snapshot"
		case strings.HasPrefix(p, "agents/"):
			if parts := strings.Split(p, "/"); len(parts) > 2 {
				return fmt.Sprintf("the agent %s", parts[1])
			}
		case strings.HasPrefix(p, "models/"):
			return "which language models agents use"
		case strings.HasPrefix(p, "secrets/"):
			return "an app's secrets, encrypted"
		}
	}
	switch {
	case strings.HasPrefix(p, ".gitea/workflows/"):
		return "a pipeline or scheduled job of the repo"
	case strings.HasSuffix(base, ".png") || strings.HasSuffix(base, ".jpg") || strings.HasSuffix(base, ".jpeg"):
		return "a picture"
	case strings.EqualFold(base, "README.md"):
		return "the repo's description"
	case base == "Dockerfile":
		return "how the app's image is built"
	}
	return ""
}

// Verb says what happens to a file, in a word.
func (f File) Verb() string {
	switch f.Status {
	case "added":
		return "adds"
	case "removed", "deleted":
		return "removes"
	case "renamed":
		return "renames"
	}
	return "changes"
}

// Deploys tells whether merging it deploys something: a change to the
// config's stacks, or to a branch apps are deployed from.
func (c Card) Deploys() bool {
	if c.Repo == "config" {
		for _, f := range c.Files {
			if strings.HasPrefix(f.Path, "stacks/") || strings.HasPrefix(f.Path, "restores/") {
				return true
			}
		}
		return false
	}
	return c.Base == "main" || c.Base == "dev"
}

// Checks' combined state: "passed", "running", "failed" or "" if it has
// none.
func (c Card) CheckState() string {
	if len(c.Checks) == 0 {
		return ""
	}
	state := "passed"
	for _, k := range c.Checks {
		switch k.State {
		case "error", "failure":
			return "failed"
		case "pending", "":
			state = "running"
		}
	}
	return state
}

// Open tells whether it is still open.
func (c Card) Open() bool { return !c.Merged && !c.Closed }

// Waits says in a sentence what it waits for, from the person's side.
func (c Card) Waits() string {
	switch {
	case c.Merged:
		return "It is merged."
	case c.Closed:
		return "It was closed without merging."
	}
	checks := c.CheckState()
	switch {
	case checks == "failed":
		return "A check failed: see below. It cannot be merged like this."
	case c.NoApproval && checks != "running":
		return "It needs no other approval here: it can be merged."
	case c.NoApproval && c.Mine:
		return "It needs no other approval here; its checks are still running."
	case !c.Mine && !c.Approved:
		return "It waits for an approval: yours, if you may give it."
	case len(c.Approvals) == 0:
		return "It waits for an approval from someone who did not propose it."
	case checks == "running":
		return "Approved; its checks are still running."
	}
	return "Approved and checked: it can be merged."
}

// Ready tells whether it looks ready to merge: approved, and no check
// failed or running. Gitea decides; this is what the card offers first.
func (c Card) Ready() bool {
	s := c.CheckState()
	return c.Open() && (len(c.Approvals) > 0 || c.NoApproval) && (s == "passed" || s == "")
}

// Plain turns Gitea's refusal of an approval or a merge into words for the
// person who clicked.
func Plain(err error) string {
	if err == nil {
		return ""
	}
	m := err.Error()
	switch {
	case strings.Contains(m, "enough approvals"):
		return "It needs an approval first, from someone allowed to approve here who did not propose it."
	case strings.Contains(m, "status check"), strings.Contains(m, "checks"):
		return "Its checks have not all passed yet."
	case strings.Contains(m, "approve your own"), strings.Contains(m, "review your own"):
		return "You proposed it, so someone else approves it."
	case strings.Contains(m, "forbidden"), strings.Contains(m, "not allowed"), strings.Contains(m, "permission"):
		return "You may not do that here."
	case strings.Contains(m, "try again later"):
		return "Gitea is still checking it; try again in a moment."
	case strings.Contains(m, "conflict"):
		return "It conflicts with what changed meanwhile; ask the agent to propose it again."
	}
	return "Gitea said: " + m
}
