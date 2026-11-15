package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"garrison/control-plane/internal/agents"
	"garrison/control-plane/internal/events"
)

// changing are the tools whose calls the feed gets: those that change
// something. Reads go to the feed only when they are refused.
var changing = map[string]bool{
	"propose_change":   true,
	"propose_patch":    true,
	"merge_change":     true,
	"screenshot":       true,
	"create_namespace": true,
	"create_repo":      true,
}

var (
	pullLink = regexp.MustCompile(`https?://[^\s)"']+/pulls/(\d+)`)
	anyLink  = regexp.MustCompile(`https?://[^\s)"']+`)
)

// toolEvent is the feed's record of a tool call, or false if it gets none.
// Its audience is the namespace's people, and the person it was for: what
// someone asks an agent is theirs and their team's to read.
func toolEvent(p Person, agent *agents.Agent, tool string, raw json.RawMessage, out string, err error) (events.Event, bool) {
	status := events.StatusOK
	switch {
	case err == nil:
	case errors.Is(err, ErrForbidden) || errors.As(err, new(refused)):
		status = events.StatusRefused
	default:
		status = events.StatusFailed
	}
	if status == events.StatusOK && !changing[tool] {
		return events.Event{}, false
	}
	var args struct {
		Namespace string `json:"namespace"`
		Repo      string `json:"repo"`
		Number    int    `json:"number"`
		Name      string `json:"name"`
		URL       string `json:"url"`
	}
	json.Unmarshal(raw, &args)
	e := events.Event{
		Source: events.SourceGateway, Kind: tool, Status: status,
		Namespace: args.Namespace, Repo: args.Repo, Audience: events.AudienceMembers,
		Actor: p.Name, StartedBy: p.StartedBy, PR: args.Number,
	}
	if p.Unattended {
		e.Actor = p.Owner
	}
	if agent != nil {
		e.Agent = agent.ID()
		if e.Namespace == "" {
			e.Namespace = agent.Namespace
		}
	}
	if tool == "create_namespace" && args.Name != "" {
		e.Namespace = args.Name
	}
	where := e.Namespace
	if e.Repo != "" {
		where += "/" + e.Repo
	}
	through := ""
	if e.Agent != "" {
		through = " through " + e.Agent
	}
	switch status {
	case events.StatusOK:
		e.Detail = out
		if m := pullLink.FindStringSubmatch(out); m != nil {
			e.Link = m[0]
			if n, err := strconv.Atoi(m[1]); err == nil {
				e.PR = n
			}
		} else if l := anyLink.FindString(out); l != "" {
			e.Link = l
		}
		e.Title = fmt.Sprintf("%s%s: %s on %s", e.Actor, through, strings.ReplaceAll(tool, "_", " "), where)
		if e.PR != 0 {
			e.Title += fmt.Sprintf(" #%d", e.PR)
		}
	default:
		e.Detail = err.Error()
		verb := "was refused"
		if status == events.StatusFailed {
			verb = "failed"
		}
		e.Title = fmt.Sprintf("%s%s: %s %s", e.Actor, through, strings.ReplaceAll(tool, "_", " "), verb)
		if where != "" {
			e.Title += " on " + where
		}
		if args.URL != "" {
			e.Title += " (" + args.URL + ")"
		}
	}
	if p.Unattended {
		e.Title += ", with nobody watching"
	}
	return e, true
}

// record gives the feed the call, if it gets one.
func (m *MCP) record(ctx context.Context, p Person, agent *agents.Agent, tool string, args json.RawMessage, out string, err error) {
	if m.Events == nil {
		return
	}
	if e, ok := toolEvent(p, agent, tool, args, out, err); ok {
		m.Events.Record(ctx, e)
	}
}

// Visibility is what the feed shows person: everything to a root admin
// (Gitea's admins), else the events of the repos Gitea lets them read and,
// of the namespaces they are in, those for its people.
func (g *Gitea) Visibility(ctx context.Context, person string) (events.Visibility, error) {
	v := events.Visibility{Person: person, Repos: map[string]bool{}, Members: map[string]bool{}}
	if !userName.MatchString(person) {
		return v, fmt.Errorf("%w: user %q", ErrInvalid, person)
	}
	var u struct {
		IsAdmin bool `json:"is_admin"`
	}
	if _, err := g.do(ctx, "", http.MethodGet, "/users/"+person, nil, &u); err != nil {
		return v, err
	}
	if u.IsAdmin {
		v.All = true
		return v, nil
	}
	namespaces, err := g.Namespaces(ctx, person)
	if err != nil {
		return v, err
	}
	for _, ns := range namespaces {
		repos, err := g.Repos(ctx, person, ns.Name)
		if err != nil {
			return v, err
		}
		for _, r := range repos {
			v.Repos[ns.Name+"/"+r.Name] = true
		}
		role, err := g.Role(ctx, ns.Name, person)
		if err != nil {
			return v, err
		}
		if role != "" {
			v.Members[ns.Name] = true
		}
	}
	return v, nil
}
