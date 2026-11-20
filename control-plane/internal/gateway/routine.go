package gateway

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"garrison/control-plane/internal/agents"
)

// routineEntry is one routine as add_routine writes it into routines.yaml.
type routineEntry struct {
	Agent    string `yaml:"agent,omitempty"`
	Chain    string `yaml:"chain,omitempty"`
	Name     string `yaml:"name"`
	Schedule string `yaml:"schedule,omitempty"`
	Every    string `yaml:"every,omitempty"`
	Owner    string `yaml:"owner"`
	Task     string `yaml:"task,omitempty"`
}

// grantEntry is the grant add_routine adds when the routine changes a repo.
type grantEntry struct {
	Agent      string `yaml:"agent"`
	Repo       string `yaml:"repo"`
	Level      string `yaml:"level"`
	For        string `yaml:"for"`
	Unattended bool   `yaml:"unattended"`
}

// addRoutine proposes a routine, "do this every day at 9", as a pull request
// to the unit's config: the entry is written here, not by the model,
// so it is valid YAML, keeps the routines already there, and is checked as
// the approval check will check it. A routine that changes a repo also
// needs a grant its run may use with nobody watching; that is added to
// grants.yaml in the same pull request, unless there is one.
func (t *Tools) addRoutine(ctx context.Context, p Person, agent *agents.Agent, a args) (string, error) {
	if err := validOrg(a.Unit); err != nil {
		return "", err
	}
	if a.Unit == PlatformUnit {
		// garrison's own routines, e.g. updates of the platform's software:
		// root admins only, who alone merge them anyway.
		if admin, err := t.Gitea.IsAdmin(ctx, p.Name); err != nil {
			return "", err
		} else if !admin {
			return "", fmt.Errorf("%w: only root admins add routines in garrison; name the unit the routine works in", ErrForbidden)
		}
	}
	name, which := strings.TrimSpace(a.Name), agents.NameOf(strings.TrimSpace(a.Agent))
	task, chainName := strings.TrimSpace(a.Task), strings.TrimSpace(a.Chain)
	// Who proposes into repos with nobody watching: the agent, or every
	// agent of the chain that proposes.
	proposers := []string{which}
	if chainName != "" {
		defs, err := t.Gitea.Definitions(ctx)
		if err != nil {
			return "", err
		}
		c, ok := agents.ChainNamed(defs, chainName)
		if !ok {
			return "", fmt.Errorf("%w: there is no chain %q in garrison/config chains/", ErrInvalid, chainName)
		}
		if which != "" && which != c.Lead {
			return "", fmt.Errorf("%w: the chain %s is run by its lead, %s; leave agent out", ErrInvalid, c.Name, c.Lead)
		}
		which, proposers = c.Lead, nil
		for _, n := range c.Agents() {
			if i := slices.IndexFunc(defs, func(d agents.Agent) bool { return d.Name == n }); i >= 0 &&
				slices.ContainsFunc(defs[i].Tools, func(t string) bool { return t == "propose_patch" || t == "screenshot" }) {
				proposers = append(proposers, n)
			}
		}
	}
	if name == "" || which == "" || (task == "" && chainName == "") {
		return "", fmt.Errorf("%w: a routine needs a name, and agent and task, or chain", ErrInvalid)
	}
	if (a.Schedule == "") == (a.Every == "") {
		return "", fmt.Errorf("%w: give either schedule (cron, e.g. \"0 9 * * *\" for every day at 9) or every (e.g. 6h), not both", ErrInvalid)
	}
	entry := routineEntry{Agent: which, Name: name, Schedule: strings.TrimSpace(a.Schedule), Every: strings.TrimSpace(a.Every), Owner: p.Name, Task: task}
	if chainName != "" {
		entry.Agent, entry.Chain = "", chainName
	}

	main := configFiles{g: t.Gitea, ns: a.Unit, ref: "main"}
	files := map[string]string{}
	routines, err := main.Read(ctx, agents.RoutinesFile)
	if err != nil && !errors.Is(err, agents.ErrNotExist) {
		return "", err
	}
	if files[agents.RoutinesFile], err = appendEntry(routines, entry); err != nil {
		return "", err
	}
	repos := slices.Clone(a.Repos)
	if a.Repo != "" && !slices.Contains(repos, a.Repo) {
		repos = append(repos, a.Repo) // as before repos
	}
	var gave []string
	if len(repos) > 0 {
		grants, err := main.Read(ctx, agents.GrantsFile)
		if err != nil && !errors.Is(err, agents.ErrNotExist) {
			return "", err
		}
		var have []agents.Grant
		_ = yaml.Unmarshal([]byte(grants), &have) // a broken file is caught below
		for _, repo := range repos {
			if err := validRepo(repo); err != nil {
				return "", err
			}
			for _, who := range proposers {
				if slices.ContainsFunc(have, func(g agents.Grant) bool {
					return agents.NameOf(g.Agent) == who && g.Repo == repo && g.Unattended
				}) {
					continue
				}
				if grants, err = appendEntry(grants, grantEntry{Agent: who, Repo: repo, Level: agents.LevelPropose, For: agents.ForMembers, Unattended: true}); err != nil {
					return "", err
				}
				have = append(have, agents.Grant{Agent: who, Repo: repo, Unattended: true})
				gave = append(gave, who+" in "+a.Unit+"/"+repo)
			}
		}
		if len(gave) > 0 {
			files[agents.GrantsFile] = grants
		}
	}
	granted := ""
	if len(gave) > 0 {
		granted = fmt.Sprintf(" It also gives grants to propose that its runs may use with nobody watching (grants.yaml): %s.", strings.Join(gave, ", "))
	}
	if err := t.checkFiles(ctx, a.Unit, files); err != nil {
		return "", err
	}

	as, viaAgent, err := t.proposer(ctx, p, agent, a.Unit, ConfigRepo)
	if err != nil {
		return "", err
	}
	note := ""
	if agent != nil {
		role := "member"
		if viaAgent {
			if role, err = t.role(ctx, p, a.Unit); err != nil {
				return "", err
			}
		}
		note = askedBy(p, agent, role)
	}
	when := "on the schedule " + entry.Schedule
	if entry.Every != "" {
		when = "every " + entry.Every
	}
	title := a.Title
	if strings.TrimSpace(title) == "" {
		title = fmt.Sprintf("Routine %s: %s %s", name, which, when)
	}
	what := task
	if chainName != "" {
		what = strings.TrimSpace("the chain " + chainName + ". " + task)
	}
	desc := strings.TrimSpace(a.Description + fmt.Sprintf("\n\n%s runs %s, for %s: %s", which, when, p.Name, what))
	c, err := t.Gitea.ProposeAs(ctx, as, a.Unit, ConfigRepo, files, title, desc, newBranch(p, agent, viaAgent), note)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("Opened change %s/%s#%d: the routine %s of %s, %s, for %s; what it finds is posted in %s's channel.%s Nothing runs until a person allowed to merge merges it.\n%s",
		c.Unit, c.Repo, c.Number, name, which, when, p.Name, a.Unit, granted, t.links(c)), nil
}

// appendEntry adds one entry to a YAML list file, keeping what is there,
// comments included. A list written in flow style ([...]) is written anew.
func appendEntry(file string, entry any) (string, error) {
	item, err := yaml.Marshal([]any{entry})
	if err != nil {
		return "", err
	}
	trimmed := strings.TrimSpace(file)
	if trimmed == "" || trimmed == "[]" {
		return string(item), nil
	}
	var list []any
	if err := yaml.Unmarshal([]byte(file), &list); err != nil {
		// Left as it is: the check names what is wrong with it.
		return strings.TrimRight(file, "\n") + "\n" + string(item), nil
	}
	if strings.HasPrefix(trimmed, "[") {
		out, err := yaml.Marshal(append(list, entry))
		return string(out), err
	}
	return strings.TrimRight(file, "\n") + "\n" + string(item), nil
}

// checkFiles refuses grants.yaml and routines.yaml of a unit's config
// (garrison's too),
// as they would be with files, if the approval check would refuse them.
func (t *Tools) checkFiles(ctx context.Context, ns string, files map[string]string) error {
	return t.checkBindingsAt(ctx, overlay{Files: configFiles{g: t.Gitea, ns: ns, ref: "main"}, files: files}, ns, slices.Sorted(maps.Keys(files)))
}

// ProposeAs proposes new contents of several files of one repo, as the
// person or account as: one commit on a new branch from main, and its pull
// request.
func (g *Gitea) ProposeAs(ctx context.Context, as, org, repo string, files map[string]string, title, description, branch, note string) (Change, error) {
	if err := validOrg(org); err != nil {
		return Change{}, err
	}
	if err := validRepo(repo); err != nil {
		return Change{}, err
	}
	if len(files) == 0 {
		return Change{}, fmt.Errorf("%w: no files", ErrInvalid)
	}
	if strings.TrimSpace(title) == "" {
		return Change{}, fmt.Errorf("%w: a change needs a title", ErrInvalid)
	}
	if branch == "" {
		branch = fmt.Sprintf("%s%s-%d", ChangeBranch, as, time.Now().UnixNano()/1e6)
	}
	if note != "" {
		description = strings.TrimSpace(description + "\n\n" + note)
	}
	paths := make([]string, 0, len(files))
	for p := range files {
		if err := validPath(p); err != nil || p == "" {
			return Change{}, fmt.Errorf("%w: path %q", ErrInvalid, p)
		}
		paths = append(paths, p)
	}
	sort.Strings(paths)
	var ops []map[string]string
	for _, p := range paths {
		op := map[string]string{"operation": "create", "path": p, "content": base64.StdEncoding.EncodeToString([]byte(files[p]))}
		var existing struct {
			SHA string `json:"sha"`
		}
		status, err := g.do(ctx, as, http.MethodGet, fmt.Sprintf("/repos/%s/%s/contents/%s?ref=main", org, repo, escapePath(p)), nil, &existing)
		switch {
		case err == nil && existing.SHA != "":
			op["operation"], op["sha"] = "update", existing.SHA
		case err != nil && status != http.StatusNotFound:
			return Change{}, err
		}
		ops = append(ops, op)
	}
	if _, err := g.do(ctx, as, http.MethodPost, fmt.Sprintf("/repos/%s/%s/contents", org, repo), map[string]any{
		"branch": "main", "new_branch": branch, "message": title, "files": ops,
	}, nil); err != nil {
		return Change{}, err
	}
	var pr struct {
		Number  int    `json:"number"`
		HTMLURL string `json:"html_url"`
	}
	if _, err := g.do(ctx, as, http.MethodPost, fmt.Sprintf("/repos/%s/%s/pulls", org, repo),
		map[string]any{"head": branch, "base": "main", "title": title, "body": description}, &pr); err != nil {
		return Change{}, err
	}
	return Change{Unit: org, Repo: repo, Number: pr.Number, Title: title, Body: description, Author: as, URL: pr.HTMLURL}, nil
}
