package home

import (
	"context"
	"embed"
	"html/template"
	"path"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"garrison/control-plane/internal/events"
)

// The feed's icons are Lucide's (icons/, ISC, see icons/LICENSE), put
// into each page once as an SVG sprite: each is a <symbol id="i-<name>">,
// drawn with <svg><use href="#i-<name>"/></svg> in the current colour.

//go:embed icons/*.svg
var iconFiles embed.FS

var (
	svgOpen  = regexp.MustCompile(`(?s)^.*?<svg[^>]*>`)
	svgClose = regexp.MustCompile(`(?s)</svg>\s*$`)
	sprite   = buildSprite()
)

func buildSprite() template.HTML {
	entries, err := iconFiles.ReadDir("icons")
	if err != nil {
		panic(err)
	}
	var b strings.Builder
	b.WriteString(`<svg xmlns="http://www.w3.org/2000/svg" style="display:none" aria-hidden="true">`)
	for _, e := range entries {
		raw, err := iconFiles.ReadFile("icons/" + e.Name())
		if err != nil {
			panic(err)
		}
		inner := svgClose.ReplaceAllString(svgOpen.ReplaceAllString(string(raw), ""), "")
		name := strings.TrimSuffix(e.Name(), path.Ext(e.Name()))
		b.WriteString(`<symbol id="i-` + name + `" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">`)
		b.WriteString(strings.TrimSpace(inner))
		b.WriteString(`</symbol>`)
	}
	b.WriteString(`</svg>`)
	return template.HTML(b.String()) // the files are ours, embedded
}

// kindIcons is the icon of each kind of event.
var kindIcons = map[string]string{
	events.KindPush:          "git-commit-horizontal",
	events.KindBranchCreated: "git-branch-plus",
	events.KindBranchDeleted: "trash-2",
	events.KindRepoCreated:   "folder-git-2",
	events.KindRepoDeleted:   "trash-2",
	events.KindPROpened:      "git-pull-request-create",
	events.KindPRUpdated:     "git-pull-request",
	events.KindPRMerged:      "git-merge",
	events.KindPRClosed:      "git-pull-request-closed",
	events.KindPRReopened:    "git-pull-request",
	events.KindApproved:      "circle-check",
	events.KindRejected:      "circle-x",
	events.KindPipeline:      "workflow",
	events.KindApprovalCheck: "shield-check",
	events.KindDeploy:        "rocket",
	events.KindUndeploy:      "circle-stop",
	events.KindRestore:       "archive-restore",
	events.KindRoutine:       "clock",
	events.KindAsk:           "arrow-right-left",
	events.KindAlert:         "triangle-alert",
	events.KindSecret:        "key-round",
	events.KindPeople:        "user-plus",
	events.KindModel:         "cpu",
	events.KindQuestion:      "message-circle-question",
	events.KindUnit:          "users",
	events.KindMedia:         "clapperboard",
	events.KindLesson:        "lightbulb",
}

// icon is the icon of an event: its kind's, else a brain for what an agent
// did.
func icon(e events.Event) string {
	if i, ok := kindIcons[e.Kind]; ok {
		return i
	}
	return "brain"
}

// svgIcon draws the icon name.
func svgIcon(name string) template.HTML {
	return template.HTML(`<svg class="icon" aria-hidden="true"><use href="#i-` + template.HTMLEscapeString(name) + `"/></svg>`)
}

// agentTag is how an agent shows: a brain and @name, its unit in its title.
func agentTag(id string) template.HTML {
	unit, name, ok := strings.Cut(id, "/")
	if !ok {
		name = id
	}
	title := "an agent"
	if unit != "" && ok {
		title += ", working in " + unit
	}
	return template.HTML(`<span class="ag" title="` + template.HTMLEscapeString(title) + `">` + string(svgIcon("brain")) + `@` + template.HTMLEscapeString(name) + `</span>`)
}

// personTag is how a person shows: a figure and their name.
func personTag(name string) template.HTML {
	return template.HTML(`<span class="pe" title="a person">` + string(svgIcon("user")) + template.HTMLEscapeString(name) + `</span>`)
}

// knownAgents are the names of garrison's agents, as the dashboard last
// read them (Server.agentNames); the defaults until then.
var knownAgents = struct {
	sync.Mutex
	names []string
	re    *regexp.Regexp
	at    time.Time
}{names: []string{"operator", "commander", "coder", "sentry", "scout", "designer"}}

// agentPattern matches an agent's id, <unit>/<name>, or its Gitea account,
// agent-<unit>-<name>, as a word of its own: not a repo's name with @branch
// or #number after it, nor part of a path.
func agentPattern() *regexp.Regexp {
	knownAgents.Lock()
	defer knownAgents.Unlock()
	if knownAgents.re == nil {
		names := make([]string, 0, len(knownAgents.names))
		for _, n := range knownAgents.names {
			names = append(names, regexp.QuoteMeta(n))
		}
		alt := strings.Join(names, "|")
		knownAgents.re = regexp.MustCompile(`(^|[^A-Za-z0-9_./@#-])(?:([a-z0-9][a-z0-9-]*)/(` + alt + `)|agent-([a-z0-9][a-z0-9-]*?)-(` + alt + `))($|[^A-Za-z0-9_/@#-])`)
	}
	return knownAgents.re
}

// setAgents keeps the names of garrison's agents for agentPattern.
func setAgents(names []string, at time.Time) {
	knownAgents.Lock()
	defer knownAgents.Unlock()
	if len(names) > 0 && !slices.Equal(names, knownAgents.names) {
		knownAgents.names, knownAgents.re = slices.Clone(names), nil
	}
	knownAgents.at = at
}

// titleHTML is an event's title with the agents it names shown as agents
// (their id or Gitea account becomes @name with a brain) and its person, the
// actor, as a person.
func titleHTML(e events.Event) template.HTML {
	out := template.HTMLEscapeString(e.Title)
	re := agentPattern()
	out = re.ReplaceAllStringFunc(out, func(m string) string {
		g := re.FindStringSubmatch(m)
		unit, name := g[2], g[3]
		if name == "" {
			unit, name = g[4], g[5]
		}
		return g[1] + string(agentTag(unit+"/"+name)) + g[6]
	})
	if e.Actor != "" && !strings.HasPrefix(e.Actor, "agent-") {
		out = replaceFirstWord(out, template.HTMLEscapeString(e.Actor), string(personTag(e.Actor)))
	}
	return template.HTML(out) // escaped above; the tags are ours
}

// replaceFirstWord replaces old once where it stands as a word of its own.
func replaceFirstWord(s, old, repl string) string {
	re := regexp.MustCompile(`(^|[\s(,])` + regexp.QuoteMeta(old) + `($|[\s:,.)'])`)
	loc := re.FindStringSubmatchIndex(s)
	if loc == nil {
		return s
	}
	return s[:loc[3]] + repl + s[loc[3]+len(old):]
}

// agentNames reads the names of garrison's agents for the titles, in the
// background, at most every 5 minutes; until then the titles use those it
// knew.
func (s *Server) agentNames() {
	if s.Providers == nil {
		return
	}
	knownAgents.Lock()
	due := time.Since(knownAgents.at) > 5*time.Minute
	if due {
		knownAgents.at = time.Now() // one reader at a time
	}
	knownAgents.Unlock()
	if !due {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		list, err := s.Providers.Agents(ctx)
		if err != nil {
			return
		}
		names := make([]string, 0, len(list))
		for _, a := range list {
			names = append(names, a.Name)
		}
		slices.Sort(names)
		setAgents(names, time.Now())
	}()
}
