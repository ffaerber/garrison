package gateway

import (
	"context"
	"crypto/hmac"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
)

// unit.md describes a unit as it is now, generated each time it is read
// from Git and the cluster, never written by hand: its people, apps (where
// they answer, at which commit, whether they run), repos with their last
// change, and the agents' rights and routines there. As AGENTS.md describes
// a repository for coding agents, unit.md describes a unit for agents and
// people (docs/ontology.md). It is read as a person: what they may see.

// MaxUnitMD is how long a unit.md may get; what is longer is cut.
const MaxUnitMD = 8000

// UnitMD is unit.md of the unit ns, as person as may see it. A unit they
// cannot see is ErrForbidden.
func (t *Tools) UnitMD(ctx context.Context, as, ns string) (string, error) {
	if err := validOrg(ns); err != nil {
		return "", err
	}
	g := t.Gitea
	units, err := g.Units(ctx, as)
	if err != nil {
		return "", err
	}
	i := slices.IndexFunc(units, func(u Unit) bool { return u.Name == ns })
	if i < 0 {
		return "", fmt.Errorf("%w: %s cannot see the unit %s", ErrForbidden, as, ns)
	}
	var b strings.Builder
	title := ns
	if d := strings.TrimSpace(units[i].Description); d != "" {
		title += ": " + d
	}
	fmt.Fprintf(&b, "# %s\n\n", title)
	if ns == PlatformUnit {
		b.WriteString("The platform itself: Garrison's own stacks, its agents (garrison/config) and models. Root admins change it.\n\n")
	} else if raw, err := g.Read(ctx, as, ns, ConfigRepo, MembersFile); err == nil {
		if r, err := ParseRoster([]byte(raw)); err == nil {
			fmt.Fprintf(&b, "People: %s (admins)", strings.Join(r.Admins, ", "))
			if len(r.Members) > 0 {
				fmt.Fprintf(&b, "; %s (members)", strings.Join(r.Members, ", "))
			}
			b.WriteString(". Admins approve and merge changes; members push to its repos.\n\n")
		}
	}

	// Apps: every environment deployed, where it answers and whether it runs.
	stacks, err := t.Stacks(ctx, ns)
	b.WriteString("## Apps\n\n")
	switch {
	case err != nil:
		fmt.Fprintf(&b, "Cannot be read right now: %v\n\n", err)
	case len(stacks) == 0:
		b.WriteString("None deployed. Each file stacks/<name>.yaml of its repo config is an app.\n\n")
	default:
		for _, s := range stacks {
			fmt.Fprintf(&b, "- %s", s.Name)
			if len(s.Addresses) > 0 {
				fmt.Fprintf(&b, ": %s", strings.Join(s.Addresses, " "))
			}
			if s.Commit != "" {
				fmt.Fprintf(&b, ", at %.7s", s.Commit)
			}
			fmt.Fprintf(&b, ", %s\n", health(s.Services))
		}
		b.WriteString("\n")
	}

	// Repos, with their last change on main.
	repos, err := g.Repos(ctx, as, ns)
	if err == nil && len(repos) > 0 {
		b.WriteString("## Repos\n\n")
		for _, r := range repos {
			if r.Archived {
				continue
			}
			fmt.Fprintf(&b, "- %s", r.Name)
			if d := strings.TrimSpace(r.Description); d != "" {
				fmt.Fprintf(&b, ": %s", d)
			}
			if c, ok := g.lastCommit(ctx, as, ns, r.Name); ok {
				fmt.Fprintf(&b, " (last change: %q by %s, %s)", c.title, c.author, agoText(c.at))
			}
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}

	// The agents' rights and routines here.
	list, _, err := g.Agents(ctx, ns, "main")
	if err == nil {
		var lines []string
		for _, a := range list {
			var parts []string
			for _, gr := range a.Grants {
				what := gr.Level + " on " + gr.Repo
				if gr.Branch != "" {
					what += " into " + gr.Branch
				}
				if gr.For != "" {
					what += ", for " + gr.For
				}
				parts = append(parts, what)
			}
			for _, r := range a.Routines {
				when := r.Schedule
				if when == "" && r.Raw != "" {
					when = "every " + r.Raw
				}
				if r.OnPush != nil {
					when = "on a push"
				}
				parts = append(parts, fmt.Sprintf("routine %s (%s)", r.Name, strings.TrimSpace(when)))
			}
			if len(parts) > 0 {
				lines = append(lines, fmt.Sprintf("- @%s: %s", a.Name, strings.Join(parts, "; ")))
			}
		}
		if len(lines) > 0 {
			b.WriteString("## Agents' rights and routines here\n\n")
			b.WriteString(strings.Join(lines, "\n"))
			b.WriteString("\n\nEvery other agent works here too, with the rights of the person it works for.\n")
		}
	}
	out := b.String()
	if len(out) > MaxUnitMD {
		out = out[:MaxUnitMD] + "\n… (cut)\n"
	}
	return out, nil
}

// health says in words whether an environment's services run.
func health(services []Service) string {
	if len(services) == 0 {
		return "not running"
	}
	var down []string
	for _, s := range services {
		ready, want, ok := strings.Cut(s.Replicas, "/")
		if !ok || ready != want || want == "0" {
			down = append(down, s.Name+" "+s.Replicas)
		}
	}
	if len(down) == 0 {
		return "running"
	}
	return "not all running: " + strings.Join(down, ", ")
}

type commitInfo struct {
	title, author string
	at            time.Time
}

// lastCommit is the newest commit of a repo's default branch, as person as.
func (g *Gitea) lastCommit(ctx context.Context, as, org, repo string) (commitInfo, bool) {
	var list []struct {
		Commit struct {
			Message string `json:"message"`
			Author  struct {
				Name string    `json:"name"`
				Date time.Time `json:"date"`
			} `json:"author"`
		} `json:"commit"`
		Author *struct {
			Login string `json:"login"`
		} `json:"author"`
	}
	if _, err := g.do(ctx, as, http.MethodGet, fmt.Sprintf("/repos/%s/%s/commits?limit=1&stat=false&verification=false&files=false", org, url.PathEscape(repo)), nil, &list); err != nil || len(list) == 0 {
		return commitInfo{}, false
	}
	c := list[0]
	title, _, _ := strings.Cut(strings.TrimSpace(c.Commit.Message), "\n")
	if len(title) > 100 {
		title = title[:100] + "…"
	}
	who := c.Commit.Author.Name
	if c.Author != nil && c.Author.Login != "" {
		who = c.Author.Login
	}
	return commitInfo{title: title, author: who, at: c.Commit.Author.Date}, true
}

// agoText is how long ago t was, in words.
func agoText(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < 2*time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%d minutes ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d hours ago", int(d.Hours()))
	}
	return fmt.Sprintf("%d days ago", int(d.Hours()/24))
}

// PersonUnitsMD is a short unit.md of every unit person is in, but
// garrison: what an agent in a private chat, which belongs to no unit,
// needs to know what "my unit" means.
func (t *Tools) PersonUnitsMD(ctx context.Context, person string) (string, error) {
	units, err := t.Gitea.Units(ctx, person)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	n := 0
	for _, u := range units {
		if u.Name == PlatformUnit {
			continue
		}
		if !t.inUnit(ctx, person, u.Name) {
			continue
		}
		md, err := t.UnitMD(ctx, person, u.Name)
		if err != nil {
			continue
		}
		b.WriteString(md)
		b.WriteString("\n")
		if n++; n >= 5 {
			b.WriteString("(and more units, not shown)\n")
			break
		}
	}
	if n == 0 {
		return fmt.Sprintf("%s is in no unit besides garrison.\n", person), nil
	}
	return b.String(), nil
}

// inUnit tells whether person is an admin or member of the unit ns (by its
// members.yaml), not only someone who may see it.
func (t *Tools) inUnit(ctx context.Context, person, ns string) bool {
	raw, err := t.Gitea.Read(ctx, person, ns, ConfigRepo, MembersFile)
	if err != nil {
		return false
	}
	r, err := ParseRoster([]byte(raw))
	return err == nil && r.Role(person) != ""
}

// UnitMDHandler serves the agent service, with the service key:
// GET /internal/units/{unit}/unit.md?for=<person>, the unit as that person
// may see it, and GET /internal/people/{person}/units.md, the units they are
// in.
func UnitMDHandler(t *Tools, key string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if key == "" || !hmac.Equal([]byte(r.Header.Get(ServiceHeader)), []byte(key)) {
			http.Error(w, "only for the agent service and the dashboard", http.StatusUnauthorized)
			return
		}
		var md string
		var err error
		if p := r.PathValue("person"); p != "" {
			if !userName.MatchString(p) {
				http.Error(w, "not a person", http.StatusBadRequest)
				return
			}
			md, err = t.PersonUnitsMD(r.Context(), p)
		} else {
			who := r.URL.Query().Get("for")
			if !userName.MatchString(who) {
				http.Error(w, "for whom?", http.StatusBadRequest)
				return
			}
			md, err = t.UnitMD(r.Context(), who, r.PathValue("unit"))
		}
		switch {
		case err != nil && strings.Contains(err.Error(), ErrForbidden.Error()):
			http.Error(w, err.Error(), http.StatusForbidden)
			return
		case err != nil:
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		w.Write([]byte(md))
	})
}
