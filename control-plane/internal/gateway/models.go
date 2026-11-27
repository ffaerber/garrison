package gateway

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"garrison/control-plane/internal/agents"
	"garrison/control-plane/internal/models"
)

// LLM is Garrison's models: llm/providers.yaml and llm/models.yaml of
// garrison's config repo.
type LLM struct {
	Providers models.Providers
	Catalog   models.Catalog
	Problems  []string
}

// LLM reads the model files of garrison's config at ref; ok is false if they
// are not there yet (an installation from before them, until the agent
// service writes them).
func (g *Gitea) LLM(ctx context.Context, ref string) (LLM, bool, error) {
	read := func(path string) (string, bool, error) {
		var out string
		status, err := g.do(ctx, "", http.MethodGet, fmt.Sprintf("/repos/%s/%s/raw/%s?ref=%s", models.Unit, models.Repo, escapePath(path), url.QueryEscape(ref)), nil, &out)
		if status == http.StatusNotFound {
			return "", false, nil
		}
		return out, err == nil, err
	}
	rawP, okP, err := read(models.ProvidersFile)
	if err != nil {
		return LLM{}, false, err
	}
	rawM, okM, err := read(models.ModelsFile)
	if err != nil {
		return LLM{}, false, err
	}
	if !okP && !okM {
		return LLM{}, false, nil
	}
	var l LLM
	var why []string
	l.Providers, why = models.ParseProviders(rawP)
	l.Problems = append(l.Problems, why...)
	l.Catalog, why = models.ParseCatalog(rawM)
	l.Problems = append(l.Problems, why...)
	if len(l.Problems) == 0 {
		l.Problems = models.Check(l.Providers, l.Catalog)
	}
	return l, true, nil
}

// modelProblems are the agents of a unit that name a model agents may
// not use. The models are garrison's at ref for garrison's own pull requests,
// which may change them, and on main for the others.
func (g *Gitea) modelProblems(ctx context.Context, ns, ref string, list []agents.Agent) ([]string, error) {
	if ns != models.Unit {
		ref = "main"
	}
	l, ok, err := g.LLM(ctx, ref)
	if err != nil || !ok {
		return nil, err
	}
	if ns == models.Unit && len(l.Problems) > 0 {
		return l.Problems, nil
	}
	if len(l.Problems) > 0 || ns != models.Unit {
		// garrison's to fix, where the agents are; they fall back to the
		// default meanwhile.
		return nil, nil
	}
	var out []string
	for _, a := range list {
		if why := models.Why(l.Providers, l.Catalog, a.Model); why != "" {
			out = append(out, fmt.Sprintf("%s/%s: %s", agents.Dir, a.Name, why))
		}
	}
	return out, nil
}

// listModels is the tool list_models.
func (t *Tools) listModels(ctx context.Context) (string, error) {
	l, ok, err := t.Gitea.LLM(ctx, "main")
	if err != nil {
		return "", err
	}
	if !ok {
		return "Garrison has no model list yet: garrison/config has no llm/models.yaml. Agents use the default model.", nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Models of Garrison, from garrison/config llm/models.yaml (root admins change it by pull request).\nThe default, for agents whose agent.yaml has model empty: %s\n\nEnabled, for model: in agent.yaml:\n", l.Providers.Default)
	var off, gone []string
	for _, id := range sortedKeys(l.Catalog) {
		m := l.Catalog[id]
		switch {
		case m.Gone != "":
			gone = append(gone, fmt.Sprintf("%s (since %s)", id, m.Gone))
		case m.Enabled:
			fmt.Fprintf(&b, "- %s\n", id)
		default:
			off = append(off, id)
		}
	}
	if len(off) > 0 {
		fmt.Fprintf(&b, "\nOffered by the providers but not enabled (a root admin may enable one in llm/models.yaml): %s\n", strings.Join(off, ", "))
	}
	if len(gone) > 0 {
		fmt.Fprintf(&b, "\nRetired; agents naming one use the default: %s\n", strings.Join(gone, ", "))
	}
	if len(l.Problems) > 0 {
		fmt.Fprintf(&b, "\nProblems: %s\n", strings.Join(l.Problems, "; "))
	}
	return b.String(), nil
}

// ProposeProvider proposes llm/providers.yaml of garrison's config with an
// LLM provider added, as the person as, who must be a root admin. Its API
// key is never part of it: the dashboard's Models page keeps that. note, if
// set, ends the pull request's description (whom it was asked for).
func (g *Gitea) ProposeProvider(ctx context.Context, as, name, apiURL string, include []string, note string) (Change, error) {
	if err := models.CheckProvider(name, apiURL, include); err != nil {
		return Change{}, fmt.Errorf("%w: %s", ErrInvalid, strings.TrimPrefix(err.Error(), models.ErrInvalidProvider.Error()+": "))
	}
	if admin, err := g.IsAdmin(ctx, as); err != nil {
		return Change{}, err
	} else if !admin {
		return Change{}, fmt.Errorf("%w: only root admins add LLM providers", ErrForbidden)
	}
	var raw string
	status, err := g.do(ctx, "", http.MethodGet, fmt.Sprintf("/repos/%s/%s/raw/%s?ref=main", models.Unit, models.Repo, escapePath(models.ProvidersFile)), nil, &raw)
	if status == http.StatusNotFound {
		return Change{}, fmt.Errorf("%w: garrison/config has no %s yet; the agent service writes it once it starts", ErrInvalid, models.ProvidersFile)
	}
	if err != nil {
		return Change{}, err
	}
	content, err := models.AddProvider(raw, name, apiURL, include)
	if err != nil {
		return Change{}, fmt.Errorf("%w: %s", ErrInvalid, strings.TrimPrefix(err.Error(), models.ErrInvalidProvider.Error()+": "))
	}
	desc := fmt.Sprintf("Adds the LLM provider %s, %s, to %s. Once it is merged, the model sync asks it for its models within a few minutes and lists them in %s, turned off; choose those agents use on the dashboard's Models page.\n\nIts API key is not in this change: a root admin sets it on the dashboard's Models page, which keeps it encrypted.", name, apiURL, models.ProvidersFile, models.ModelsFile)
	return g.Propose(ctx, as, models.Unit, models.Repo, models.ProvidersFile, string(content), "Add the LLM provider "+name, desc, "", note)
}

// ProposeModels proposes, as the root admin as, the models agents use, in
// one pull request: def as the default model (providers.yaml), and the agents
// of choose, by name, on another (their agent.yaml's model; "" for the
// default). Each model chosen is <provider>/<model> of models.yaml that its
// provider still offers; the ones not on yet are turned on with it.
// Agents left out keep theirs. Nothing to change is ErrInvalid.
func (g *Gitea) ProposeModels(ctx context.Context, as, def string, choose map[string]string, note string) (Change, error) {
	if admin, err := g.IsAdmin(ctx, as); err != nil {
		return Change{}, err
	} else if !admin {
		return Change{}, fmt.Errorf("%w: only root admins choose the models", ErrForbidden)
	}
	l, ok, err := g.LLM(ctx, "main")
	if err != nil {
		return Change{}, err
	}
	if !ok {
		return Change{}, fmt.Errorf("%w: garrison/config has no %s yet; the agent service writes it once it starts", ErrInvalid, models.ProvidersFile)
	}
	offered := func(id string) error {
		if m, ok := l.Catalog[id]; !ok || m.Gone != "" {
			return fmt.Errorf("%w: %s is not a model the providers offer (%s)", ErrInvalid, id, models.ModelsFile)
		}
		return nil
	}
	files := map[string]string{}
	var enable, said []string
	if def == "" {
		def = l.Providers.Default
	}
	if def != l.Providers.Default {
		if err := offered(def); err != nil {
			return Change{}, err
		}
		raw, err := g.File(ctx, models.Unit, models.Repo, models.ProvidersFile)
		if err != nil {
			return Change{}, err
		}
		out, err := models.SetDefault(raw, def)
		if err != nil {
			return Change{}, fmt.Errorf("%w: %v", ErrInvalid, err)
		}
		files[models.ProvidersFile] = string(out)
		said = append(said, "the default "+def)
	}
	enable = append(enable, def)
	defs, err := g.Definitions(ctx)
	if err != nil {
		return Change{}, err
	}
	known := map[string]agents.Agent{}
	for _, a := range defs {
		known[a.Name] = a
	}
	for _, name := range sortedKeys(choose) {
		a, ok := known[name]
		if !ok {
			return Change{}, fmt.Errorf("%w: garrison has no agent %s", ErrInvalid, name)
		}
		want := choose[name]
		if want != "" {
			if err := offered(want); err != nil {
				return Change{}, err
			}
			enable = append(enable, want)
		}
		have := ""
		if a.Model != "" {
			_, _, have = l.Providers.Resolve(a.Model)
		}
		if have == want {
			continue
		}
		path := fmt.Sprintf("%s/%s/agent.yaml", agents.Dir, name)
		raw, err := g.File(ctx, models.Unit, models.Repo, path)
		if err != nil {
			return Change{}, err
		}
		out, err := models.SetAgentModel(path, raw, want)
		if err != nil {
			return Change{}, fmt.Errorf("%w: %v", ErrInvalid, err)
		}
		files[path] = string(out)
		if want == "" {
			said = append(said, name+" on the default")
		} else {
			said = append(said, name+" on "+want)
		}
	}
	if next, changed := l.Catalog.Enable(enable...); changed {
		files[models.ModelsFile] = string(next.Marshal())
	}
	if len(files) == 0 {
		return Change{}, fmt.Errorf("%w: these are the models already in use; nothing to change", ErrInvalid)
	}
	title := "Models: " + strings.Join(said, ", ")
	if len(said) == 0 {
		title = "Models: turn on " + def
	}
	if len(title) > 200 {
		title = title[:200]
	}
	var b strings.Builder
	b.WriteString("Chosen on the dashboard's Models page.\n\n")
	if _, ok := files[models.ProvidersFile]; ok {
		fmt.Fprintf(&b, "- **Default model**, for every agent whose `agent.yaml` names none: %s (was %s)\n", def, l.Providers.Default)
	}
	for _, name := range sortedKeys(choose) {
		if _, ok := files[fmt.Sprintf("%s/%s/agent.yaml", agents.Dir, name)]; !ok {
			continue
		}
		if choose[name] == "" {
			fmt.Fprintf(&b, "- **%s** uses the default model\n", name)
		} else {
			fmt.Fprintf(&b, "- **%s** uses %s\n", name, choose[name])
		}
	}
	if _, ok := files[models.ModelsFile]; ok {
		b.WriteString("\nModels chosen that were turned off are turned on in `" + models.ModelsFile + "`.\n")
	}
	b.WriteString("\nOnce merged, the agents use them from their next turn.")
	return g.ProposeAs(ctx, as, models.Unit, models.Repo, files, title, b.String(), "", note)
}

// ProposeFiles proposes files (nil: deletes) for a repo's main branch as a
// pull request by the platform's account, on a new branch named prefix-<time>:
// an open pull request of such a branch with the same files is left as it
// is, one with others is closed, its branch deleted, and a new one opened
// from main. A branch name is never used twice: Gitea closes the pull
// requests of a deleted branch some moments later, which would take a new
// one of the same name with it. It returns the pull request's link. Without
// files, it closes the open pull requests of prefix.
func (g *Gitea) ProposeFiles(ctx context.Context, org, repo, prefix, title, body string, files map[string][]byte) (string, error) {
	if err := validOrg(org); err != nil {
		return "", err
	}
	if err := validRepo(repo); err != nil {
		return "", err
	}
	base := fmt.Sprintf("/repos/%s/%s", org, repo)
	var prs []struct {
		Number  int    `json:"number"`
		HTMLURL string `json:"html_url"`
		Head    struct {
			Ref string `json:"ref"`
		} `json:"head"`
	}
	if _, err := g.do(ctx, "", http.MethodGet, base+"/pulls?state=open&limit=50", nil, &prs); err != nil {
		return "", err
	}
	for _, pr := range prs {
		if !strings.HasPrefix(pr.Head.Ref, prefix+"-") {
			continue
		}
		branch := pr.Head.Ref
		same := len(files) > 0
		for path, want := range files {
			var have string
			status, err := g.do(ctx, "", http.MethodGet, fmt.Sprintf("%s/raw/%s?ref=%s", base, escapePath(path), url.QueryEscape(branch)), nil, &have)
			if err != nil && status != http.StatusNotFound {
				return "", err
			}
			if (want == nil) != (status == http.StatusNotFound) || (want != nil && have != string(want)) {
				same = false
				break
			}
		}
		if same {
			return pr.HTMLURL, nil
		}
		if _, err := g.do(ctx, "", http.MethodPatch, fmt.Sprintf("%s/pulls/%d", base, pr.Number), map[string]string{"state": "closed"}, nil); err != nil {
			return "", err
		}
		if status, err := g.do(ctx, "", http.MethodDelete, base+"/branches/"+escapePath(branch), nil, nil); err != nil && status != http.StatusNotFound {
			return "", err
		}
	}
	if len(files) == 0 {
		return "", nil
	}
	branch := fmt.Sprintf("%s-%d", prefix, time.Now().UnixMilli())
	ops, err := g.fileOps(ctx, org, repo, files)
	if err != nil || len(ops) == 0 {
		return "", err
	}
	for _, op := range ops {
		if c, ok := op["content"]; ok {
			op["content"] = base64.StdEncoding.EncodeToString([]byte(c))
		}
	}
	if _, err := g.do(ctx, "", http.MethodPost, base+"/contents", map[string]any{
		"branch": "main", "new_branch": branch, "message": title, "files": ops,
	}, nil); err != nil {
		return "", err
	}
	var pr struct {
		HTMLURL string `json:"html_url"`
	}
	if _, err := g.do(ctx, "", http.MethodPost, base+"/pulls", map[string]any{"head": branch, "base": "main", "title": title, "body": body}, &pr); err != nil {
		return "", err
	}
	return pr.HTMLURL, nil
}

// fileOps are the operations of a change-files request that write files
// (nil: delete) on main, with content in clear; files as they are left out.
func (g *Gitea) fileOps(ctx context.Context, org, repo string, files map[string][]byte) ([]map[string]string, error) {
	var ops []map[string]string
	for _, path := range sortedKeys(files) {
		if err := validPath(path); err != nil || path == "" {
			return nil, fmt.Errorf("%w: path %q", ErrInvalid, path)
		}
		var have struct {
			SHA string `json:"sha"`
		}
		status, err := g.do(ctx, "", http.MethodGet, fmt.Sprintf("/repos/%s/%s/contents/%s?ref=main", org, repo, escapePath(path)), nil, &have)
		if err != nil && status != http.StatusNotFound {
			return nil, err
		}
		op := map[string]string{"path": path}
		switch {
		case files[path] == nil && have.SHA == "":
			continue // already gone
		case files[path] == nil:
			op["operation"], op["sha"] = "delete", have.SHA
		case have.SHA == "":
			op["operation"] = "create"
		default:
			op["operation"], op["sha"] = "update", have.SHA
		}
		if files[path] != nil {
			op["content"] = string(files[path])
		}
		ops = append(ops, op)
	}
	return ops, nil
}
