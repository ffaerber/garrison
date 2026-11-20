package agents

import (
	"fmt"
	"garrison/control-plane/internal/cron"
	"regexp"
	"slices"
	"strings"
	"time"
)

// VisibleMembers in agent.yaml (visible) keeps an agent to its
// unit's people (admins, members and root admins) even if all its
// repos are public. Without it, an agent is for everyone unless it works on
// a private repo: see the gateway's AgentPrivate.
const VisibleMembers = "members"

// Levels of a grant, as the spec's autonomy levels: each includes the one
// before.
const (
	LevelPropose    = "propose"    // push its own branches and open pull requests
	LevelMerge      = "merge"      // also merge them, once the checks pass
	LevelAutonomous = "autonomous" // also on branches that deploy
)

// Groups a grant can be for: whose requests may use it.
const (
	ForAdmins   = "admins"
	ForMembers  = "members" // admins included
	ForEveryone = "everyone"
)

// Rank orders the levels; 0 for an unknown one.
func Rank(level string) int {
	return slices.Index([]string{LevelPropose, LevelMerge, LevelAutonomous}, level) + 1
}

// Deploying are the branches that deploy when they change: main, and dev
// of apps. merge on them would deploy without the person approving it that
// the level promises, so a grant must say autonomous.
var Deploying = []string{"main", "dev"}

// Grant is a right of an agent's own, in one repo of the unit whose
// grants.yaml gives it.
type Grant struct {
	Agent      string   `yaml:"agent"` // which of garrison's agents
	Repo       string   `yaml:"repo"`
	Branch     string   `yaml:"branch"`     // the branch it may merge into; empty for propose
	Level      string   `yaml:"level"`      // propose, merge or autonomous
	For        string   `yaml:"for"`        // admins, members (default) or everyone
	Checks     []string `yaml:"checks"`     // commit statuses that must be green before a merge
	Unattended bool     `yaml:"unattended"` // may runs without a person use it
}

// Routine is work an agent starts by itself in the unit whose
// routines.yaml names it, for its owner: every so often, or when a branch
// moves.
type Routine struct {
	Agent    string         `yaml:"agent"` // which of garrison's agents; with chain, its lead by default
	Chain    string         `yaml:"chain"` // a chain of command it runs, from garrison's chains/
	Name     string         `yaml:"name"`
	Every    time.Duration  `yaml:"-"`
	Raw      string         `yaml:"every"`
	Schedule string         `yaml:"schedule"` // cron: minute hour day month weekday, in Garrison's time zone
	Cron     *cron.Schedule `yaml:"-"`
	OnPush   *Push          `yaml:"on_push"`
	// OnAlert starts it when an alert of its unit fires whose labels
	// have these values, e.g. {alertname: ServiceDown}; {} for every alert.
	OnAlert map[string]string `yaml:"on_alert"`
	Owner   string            `yaml:"owner"` // a person of the unit; grants are checked for them
	Task    string            `yaml:"task"`  // the instruction, in words
}

// Push names a branch whose new commits start a routine.
type Push struct {
	Repo   string `yaml:"repo"`
	Branch string `yaml:"branch"`
}

var labelName = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]{0,63}$`)

// Limits of routines.
const (
	MinEvery = 10 * time.Minute
	MaxEvery = 7 * 24 * time.Hour
)

var (
	agentID  = regexp.MustCompile(`^([A-Za-z0-9._-]{1,64})/([a-z0-9]+(-[a-z0-9]+)*)$`)
	branchRe = regexp.MustCompile(`^[A-Za-z0-9._/-]{1,200}$`)
	person   = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)
	context_ = regexp.MustCompile(`^[^\x00-\x1f]{1,200}$`)
)

// ConfigRepo is the unit's config repo, which no grant may name: an
// agent must never change its own grants.
const ConfigRepo = "config"

func checkGrants(gs []Grant) (out []Grant, why []string) {
	for i, g := range gs {
		at := fmt.Sprintf("grants[%d]", i)
		if g.For == "" {
			g.For = ForMembers
		}
		switch {
		case !repoName.MatchString(g.Repo) || g.Repo == "." || g.Repo == "..":
			why = append(why, fmt.Sprintf("%s: %q is not a repo name", at, g.Repo))
		case g.Repo == ConfigRepo:
			why = append(why, at+": no agent may have a grant on config, where its own grants are")
		}
		switch g.Level {
		case LevelPropose:
			if g.Branch != "" {
				why = append(why, at+": propose takes no branch: the agent proposes onto any branch, as pull requests")
			}
		case LevelMerge, LevelAutonomous:
			if g.Branch == "" {
				why = append(why, fmt.Sprintf("%s: %s needs the branch it merges into", at, g.Level))
			} else if !branchRe.MatchString(g.Branch) || strings.Contains(g.Branch, "..") {
				why = append(why, fmt.Sprintf("%s: %q is not a branch name", at, g.Branch))
			}
			if g.Level == LevelMerge && slices.Contains(Deploying, g.Branch) {
				why = append(why, fmt.Sprintf("%s: a merge into %s deploys it at once, as the deployer cannot yet wait for a person's approval; say level: autonomous", at, g.Branch))
			}
		default:
			why = append(why, fmt.Sprintf("%s: level must be propose, merge or autonomous, not %q", at, g.Level))
		}
		switch g.For {
		case ForAdmins, ForMembers:
		case ForEveryone:
			if g.Level != LevelPropose {
				why = append(why, at+": for: everyone is only for propose; people outside the unit never merge through its agents")
			}
		default:
			why = append(why, fmt.Sprintf("%s: for must be admins, members or everyone (groups of this unit), not %q", at, g.For))
		}
		for _, c := range g.Checks {
			if !context_.MatchString(c) {
				why = append(why, fmt.Sprintf("%s: %q is not a check name", at, c))
			}
		}
		if len(g.Checks) > 0 && g.Level == LevelPropose {
			why = append(why, at+": checks are for merges; propose has none")
		}
		out = append(out, g)
	}
	return out, why
}

func checkRoutines(rs []Routine) (out []Routine, why []string) {
	seen := map[string]bool{}
	for i, r := range rs {
		at := fmt.Sprintf("routines[%d]", i)
		if !agentName.MatchString(r.Name) || len(r.Name) > 40 {
			why = append(why, at+": name must be lowercase letters, digits and dashes, at most 40")
		} else if seen[r.Name] {
			why = append(why, fmt.Sprintf("%s: there are two routines %q", at, r.Name))
		}
		seen[r.Name] = true
		set := 0
		for _, ok := range []bool{r.Raw != "", r.Schedule != "", r.OnPush != nil, r.OnAlert != nil} {
			if ok {
				set++
			}
		}
		switch {
		case set > 1:
			why = append(why, at+": one of schedule, every, on_push and on_alert, not several")
		case r.OnAlert != nil:
			for k, v := range r.OnAlert {
				switch {
				case !labelName.MatchString(k):
					why = append(why, fmt.Sprintf("%s: on_alert: %q is not a label name", at, k))
				case k == "unit":
					why = append(why, at+": on_alert: unit is always this unit")
				case v == "" || len(v) > 200:
					why = append(why, fmt.Sprintf("%s: on_alert: %s needs a value", at, k))
				}
			}
		case r.Schedule != "":
			s, err := cron.Parse(r.Schedule)
			if err != nil {
				why = append(why, fmt.Sprintf("%s: schedule: %v", at, err))
				break
			}
			from := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			if s.Next(from, time.UTC).IsZero() {
				why = append(why, fmt.Sprintf("%s: schedule %q never comes", at, r.Schedule))
			} else if s.MinGap(from, time.UTC) < MinEvery {
				why = append(why, fmt.Sprintf("%s: schedule %q runs more often than every %s", at, r.Schedule, MinEvery))
			}
			r.Cron = s
		case r.Raw != "":
			d, err := time.ParseDuration(r.Raw)
			if err != nil {
				why = append(why, fmt.Sprintf("%s: every: %q is not a duration like 6h or 30m", at, r.Raw))
			} else if d < MinEvery || d > MaxEvery {
				why = append(why, fmt.Sprintf("%s: every must be between %s and %s", at, MinEvery, MaxEvery))
			}
			r.Every = d
		case r.OnPush != nil:
			if !repoName.MatchString(r.OnPush.Repo) || !branchRe.MatchString(r.OnPush.Branch) || strings.Contains(r.OnPush.Branch, "..") {
				why = append(why, at+": on_push needs a repo of this unit and a branch")
			}
		default:
			why = append(why, at+": when it runs is missing: schedule (cron, e.g. \"0 6 * * *\"), every (e.g. 6h), on_push (repo and branch) or on_alert (label values, e.g. {alertname: ServiceDown})")
		}
		if !person.MatchString(r.Owner) {
			why = append(why, at+": owner must be the username of a person of this unit")
		}
		if strings.TrimSpace(r.Task) == "" || len(r.Task) > 4000 {
			why = append(why, at+": task must say what to do, in at most 4000 characters")
		}
		out = append(out, r)
	}
	return out, why
}

// checkAsks checks agents in agent.yaml, the agents self may ask: names of
// garrison's agents (garrison/<name> is taken too), or any. It returns them
// as names.
func checkAsks(self string, ids []string) (out []string, why []string) {
	for _, id := range ids {
		name := id
		if m := agentID.FindStringSubmatch(id); m != nil {
			if m[1] != PlatformUnit {
				why = append(why, fmt.Sprintf("agents: %q: the agents are all garrison's; name it %s", id, m[2]))
				continue
			}
			name = m[2]
		}
		switch {
		case name == AnyAgent:
			if len(ids) > 1 {
				why = append(why, "agents: any stands alone, without other agents")
			}
		case !agentName.MatchString(name):
			why = append(why, fmt.Sprintf("agents: %q is not an agent's name", id))
		case name == self:
			why = append(why, "agents: an agent does not ask itself")
		}
		out = append(out, name)
	}
	return out, why
}

// GrantFor finds a grant of a that lets a request of someone with role
// (admin, member or "") do level in repo, merging into branch. unattended
// asks for grants a run without a person may use.
func (a Agent) GrantFor(repo, branch, level, role string, unattended bool) (Grant, bool) {
	for _, g := range a.Grants {
		if g.Repo != repo || Rank(g.Level) < Rank(level) || (unattended && !g.Unattended) {
			continue
		}
		if level != LevelPropose && g.Branch != branch {
			continue
		}
		switch g.For {
		case ForAdmins:
			if role != "admin" {
				continue
			}
		case ForMembers:
			if role != "admin" && role != "member" {
				continue
			}
		}
		return g, true
	}
	return Grant{}, false
}

// NeedsAccount tells whether the agent acts with an account of its own:
// it has grants or routines.
func (a Agent) NeedsAccount() bool { return len(a.Grants) > 0 || len(a.Routines) > 0 }

// WorksOn are the repos of its unit an agent works on there: those of
// its grants and those its routines watch. If one is private, so is the
// agent, there.
func (a Agent) WorksOn() []string {
	out := append([]string{}, a.GrantedRepos()...)
	for _, r := range a.Routines {
		if r.OnPush != nil {
			out = append(out, r.OnPush.Repo)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// GrantedRepos are the repos the agent's account may write to.
func (a Agent) GrantedRepos() []string {
	var out []string
	for _, g := range a.Grants {
		if !slices.Contains(out, g.Repo) {
			out = append(out, g.Repo)
		}
	}
	slices.Sort(out)
	return out
}

// Branch is the prefix of the branches the agent's account writes:
// agent/<name>/.
func (a Agent) Branch() string { return "agent/" + a.Name + "/" }
