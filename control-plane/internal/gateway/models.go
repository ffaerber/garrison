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

// LLM is the home cloud's models: llm/providers.yaml and llm/models.yaml of
// system's config repo.
type LLM struct {
	Providers models.Providers
	Catalog   models.Catalog
	Problems  []string
}

// LLM reads the model files of system's config at ref; ok is false if they
// are not there yet (an installation from before them, until the agent
// service writes them).
func (g *Gitea) LLM(ctx context.Context, ref string) (LLM, bool, error) {
	read := func(path string) (string, bool, error) {
		var out string
		status, err := g.do(ctx, "", http.MethodGet, fmt.Sprintf("/repos/%s/%s/raw/%s?ref=%s", models.Namespace, models.Repo, escapePath(path), url.QueryEscape(ref)), nil, &out)
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

// modelProblems are the agents of a namespace that name a model agents may
// not use. The models are system's at ref for system's own pull requests,
// which may change them, and on main for the others.
func (g *Gitea) modelProblems(ctx context.Context, ns, ref string, list []agents.Agent) ([]string, error) {
	if ns != models.Namespace {
		ref = "main"
	}
	l, ok, err := g.LLM(ctx, ref)
	if err != nil || !ok {
		return nil, err
	}
	if ns == models.Namespace && len(l.Problems) > 0 {
		return l.Problems, nil
	}
	if len(l.Problems) > 0 {
		return nil, nil // system's to fix; the agents fall back to the default meanwhile
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
		return "The home cloud has no model list yet: system/config has no llm/models.yaml. Agents use the default model.", nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Models of the home cloud, from system/config llm/models.yaml (root admins change it by pull request).\nThe default, for agents whose agent.yaml has model empty: %s\n\nEnabled, for model: in agent.yaml:\n", l.Providers.Default)
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
