package gateway

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"garrison/templates"
)

// Gitea calls Gitea's API as its admin account, acting as a given person
// through the Sudo header. Gitea then checks that person's own rights, so
// the gateway and its agent can never do more than the person could.
type Gitea struct {
	URL      string // e.g. http://gitea_gitea:3000
	User     string // admin account
	Password string
	HTTP     *http.Client

	privacy privacy  // which repos are private, for a few seconds
	pushes  pushLogs // who pushed to which branch, from the repos' feeds
}

// ErrForbidden means Gitea refused the person.
var ErrForbidden = errors.New("not allowed")

// ErrInvalid marks a request the gateway refuses before asking anyone.
var ErrInvalid = errors.New("invalid request")

var (
	repoName = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}$`)
	userName = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)
)

// validOrg accepts a namespace, i.e. a Gitea organisation name.
func validOrg(o string) error {
	if !userName.MatchString(o) || o == "." || o == ".." {
		return fmt.Errorf("%w: namespace %q", ErrInvalid, o)
	}
	return nil
}

func validRepo(r string) error {
	if !repoName.MatchString(r) || r == "." || r == ".." {
		return fmt.Errorf("%w: repo name %q", ErrInvalid, r)
	}
	return nil
}

// validPath accepts a relative path inside a repo.
func validPath(p string) error {
	if p == "" {
		return nil
	}
	if strings.HasPrefix(p, "/") || strings.Contains(p, "\\") || len(p) > 300 {
		return fmt.Errorf("%w: path %q", ErrInvalid, p)
	}
	for _, part := range strings.Split(p, "/") {
		if part == "" || part == "." || part == ".." {
			return fmt.Errorf("%w: path %q", ErrInvalid, p)
		}
	}
	return nil
}

func escapePath(p string) string {
	parts := strings.Split(p, "/")
	for i, s := range parts {
		parts[i] = url.PathEscape(s)
	}
	return strings.Join(parts, "/")
}

// do sends a request as person (empty: as the admin itself) and decodes a
// JSON answer into out, if given. It returns the status code.
func (g *Gitea) do(ctx context.Context, as, method, path string, body, out any) (int, error) {
	if as != "" && !userName.MatchString(as) {
		return 0, fmt.Errorf("invalid user %q", as)
	}
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, g.URL+"/api/v1"+path, rd)
	if err != nil {
		return 0, err
	}
	req.SetBasicAuth(g.User, g.Password)
	if as != "" {
		req.Header.Set("Sudo", as)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	client := g.HTTP
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return resp.StatusCode, err
	}
	switch {
	case resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusUnauthorized:
		return resp.StatusCode, ErrForbidden
	case resp.StatusCode >= 300:
		msg := strings.TrimSpace(string(data))
		var e struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(data, &e) == nil && e.Message != "" {
			msg = e.Message
		}
		return resp.StatusCode, fmt.Errorf("gitea %s %s: %d %s", method, path, resp.StatusCode, msg)
	}
	if out != nil {
		switch o := out.(type) {
		case *string:
			*o = string(data)
		default:
			if err := json.Unmarshal(data, out); err != nil {
				return resp.StatusCode, err
			}
		}
	}
	return resp.StatusCode, nil
}

type Namespace struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// Namespaces lists the organisations person can see: all of them, page by
// page, as the syncs treat a namespace that is not listed as gone.
func (g *Gitea) Namespaces(ctx context.Context, as string) ([]Namespace, error) {
	out := []Namespace{}
	for page := 1; page <= 100; page++ {
		var raw []struct {
			Name        string `json:"username"`
			Description string `json:"description"`
		}
		if _, err := g.do(ctx, as, http.MethodGet, fmt.Sprintf("/orgs?limit=50&page=%d", page), nil, &raw); err != nil {
			return nil, err
		}
		for _, o := range raw {
			out = append(out, Namespace{Name: o.Name, Description: o.Description})
		}
		if len(raw) < 50 {
			break
		}
	}
	return out, nil
}

type Repo struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Empty       bool   `json:"empty"`
}

// Repos lists the namespace's repos as person sees them.
func (g *Gitea) Repos(ctx context.Context, as, org string) ([]Repo, error) {
	if err := validOrg(org); err != nil {
		return nil, err
	}
	var out []Repo
	for page := 1; page < 20; page++ {
		var batch []Repo
		if _, err := g.do(ctx, as, http.MethodGet, fmt.Sprintf("/orgs/%s/repos?limit=50&page=%d", org, page), nil, &batch); err != nil {
			return nil, err
		}
		out = append(out, batch...)
		if len(batch) < 50 {
			break
		}
	}
	return out, nil
}

type Entry struct {
	Name string `json:"name"`
	Path string `json:"path"`
	Type string `json:"type"` // file, dir
	Size int    `json:"size"`
}

// List lists a directory of a repo's default branch.
func (g *Gitea) List(ctx context.Context, as, org, repo, path string) ([]Entry, error) {
	if err := validOrg(org); err != nil {
		return nil, err
	}
	if err := validRepo(repo); err != nil {
		return nil, err
	}
	if err := validPath(path); err != nil {
		return nil, err
	}
	var raw json.RawMessage
	if _, err := g.do(ctx, as, http.MethodGet, fmt.Sprintf("/repos/%s/%s/contents/%s", org, repo, escapePath(path)), nil, &raw); err != nil {
		return nil, err
	}
	var entries []Entry
	if err := json.Unmarshal(raw, &entries); err != nil {
		var one Entry
		if err := json.Unmarshal(raw, &one); err != nil {
			return nil, err
		}
		entries = []Entry{one}
	}
	return entries, nil
}

// Read returns a file of a repo's default branch.
func (g *Gitea) Read(ctx context.Context, as, org, repo, path string) (string, error) {
	if err := validOrg(org); err != nil {
		return "", err
	}
	if err := validRepo(repo); err != nil {
		return "", err
	}
	if err := validPath(path); err != nil || path == "" {
		return "", fmt.Errorf("%w: path %q", ErrInvalid, path)
	}
	var out string
	_, err := g.do(ctx, as, http.MethodGet, fmt.Sprintf("/repos/%s/%s/raw/%s", org, repo, escapePath(path)), nil, &out)
	return out, err
}

type Change struct {
	Namespace string `json:"namespace"`
	Repo      string `json:"repo"`
	Number    int    `json:"number"`
	Title     string `json:"title"`
	Body      string `json:"body"`
	Author    string `json:"author"`
	URL       string `json:"url"`
	Diff      string `json:"diff,omitempty"`
}

// CanWrite tells whether person may push to a repo, and so propose a
// change to it: ErrForbidden if not.
func (g *Gitea) CanWrite(ctx context.Context, as, org, repo string) error {
	if err := validOrg(org); err != nil {
		return err
	}
	if err := validRepo(repo); err != nil {
		return err
	}
	var r struct {
		Permissions struct {
			Push bool `json:"push"`
		} `json:"permissions"`
	}
	if status, err := g.do(ctx, as, http.MethodGet, fmt.Sprintf("/repos/%s/%s", org, repo), nil, &r); status == http.StatusNotFound {
		return ErrForbidden // Gitea hides what a person may not see
	} else if err != nil {
		return err
	}
	if !r.Permissions.Push {
		return ErrForbidden
	}
	return nil
}

// CanRead tells whether person may read a repo: ErrForbidden if not.
func (g *Gitea) CanRead(ctx context.Context, as, org, repo string) error {
	if err := validOrg(org); err != nil {
		return err
	}
	if err := validRepo(repo); err != nil {
		return err
	}
	status, err := g.do(ctx, as, http.MethodGet, fmt.Sprintf("/repos/%s/%s", org, repo), nil, nil)
	if status == http.StatusNotFound || status == http.StatusForbidden {
		return ErrForbidden // Gitea hides what a person may not see
	}
	return err
}

// ChangeBranch is the prefix of the branches agents' changes go on; only
// such branches can be added to, never main or an environment's branch.
const ChangeBranch = "change/"

var branchName = regexp.MustCompile(`^[A-Za-z0-9._/-]{1,200}$`)

// MaxPatch is the largest patch ProposePatch takes.
const MaxPatch = 8 << 20

// Proposal is a patch to propose.
type Proposal struct {
	Org, Repo   string
	Base        string   // the branch it is for; default main
	Branch      string   // an earlier change's branch to add to; empty for a new one
	Fresh       string   // the new branch's name, for a new change
	Prefixes    []string // the branches that may be added to
	Patch       string   // as git diff --binary prints it
	Title       string
	Description string
	Note        string // the gateway's line on whom it is for: in a new pull request, and a comment for each addition
}

// ProposePatch applies a patch (git diff, binary files too) as the account
// as: on a new branch from base with a new pull request, or, given branch,
// on top of an earlier change, whose pull request then shows it. Like
// Propose, it never touches base itself.
func (g *Gitea) ProposePatch(ctx context.Context, as string, pp Proposal) (Change, string, error) {
	org, repo, base, branch, patch, title, description := pp.Org, pp.Repo, pp.Base, pp.Branch, pp.Patch, pp.Title, pp.Description
	if err := g.CanWrite(ctx, as, org, repo); err != nil {
		return Change{}, "", err
	}
	if base == "" {
		base = "main"
	}
	if !branchName.MatchString(base) || strings.Contains(base, "..") {
		return Change{}, "", fmt.Errorf("%w: base branch %q", ErrInvalid, base)
	}
	if len(pp.Prefixes) == 0 {
		pp.Prefixes = []string{ChangeBranch}
	}
	if branch != "" && (!slices.ContainsFunc(pp.Prefixes, func(p string) bool { return strings.HasPrefix(branch, p) }) || !branchName.MatchString(branch) || strings.Contains(branch, "..")) {
		return Change{}, "", fmt.Errorf("%w: branch %q: only branches of earlier changes, %s..., can be added to", ErrInvalid, branch, strings.Join(pp.Prefixes, "... or "))
	}
	if strings.TrimSpace(patch) == "" {
		return Change{}, "", fmt.Errorf("%w: the patch is empty", ErrInvalid)
	}
	if len(patch) > MaxPatch {
		return Change{}, "", fmt.Errorf("%w: the patch is larger than %d MB", ErrInvalid, MaxPatch>>20)
	}
	if strings.TrimSpace(title) == "" {
		return Change{}, "", fmt.Errorf("%w: a change needs a title", ErrInvalid)
	}
	req := map[string]any{"content": patch, "message": title}
	added := branch != ""
	if !added {
		branch = pp.Fresh
		if branch == "" {
			branch = fmt.Sprintf("%s%s-%d", ChangeBranch, as, time.Now().UnixNano()/1e6)
		}
		req["branch"], req["new_branch"] = base, branch
	} else {
		req["branch"] = branch
	}
	if _, err := g.do(ctx, as, http.MethodPost, fmt.Sprintf("/repos/%s/%s/diffpatch", org, repo), req, nil); err != nil {
		return Change{}, "", fmt.Errorf("the patch does not apply: %w", err)
	}
	var prs []struct {
		Number  int    `json:"number"`
		Title   string `json:"title"`
		Body    string `json:"body"`
		HTMLURL string `json:"html_url"`
		Head    struct {
			Ref string `json:"ref"`
		} `json:"head"`
	}
	if _, err := g.do(ctx, as, http.MethodGet, fmt.Sprintf("/repos/%s/%s/pulls?state=open&limit=50", org, repo), nil, &prs); err != nil {
		return Change{}, "", err
	}
	for _, pr := range prs {
		if pr.Head.Ref == branch {
			if added && pp.Note != "" {
				g.do(ctx, as, http.MethodPost, fmt.Sprintf("/repos/%s/%s/issues/%d/comments", org, repo, pr.Number),
					map[string]string{"body": "Changes added, " + strings.ToLower(pp.Note[:1]) + pp.Note[1:]}, nil)
			}
			return Change{Namespace: org, Repo: repo, Number: pr.Number, Title: pr.Title, Body: pr.Body, Author: as, URL: pr.HTMLURL}, branch, nil
		}
	}
	var pr struct {
		Number  int    `json:"number"`
		HTMLURL string `json:"html_url"`
	}
	body := description
	if pp.Note != "" {
		body = strings.TrimSpace(description + "\n\n" + pp.Note)
	}
	if _, err := g.do(ctx, as, http.MethodPost, fmt.Sprintf("/repos/%s/%s/pulls", org, repo),
		map[string]any{"head": branch, "base": base, "title": title, "body": body}, &pr); err != nil {
		return Change{}, "", err
	}
	return Change{Namespace: org, Repo: repo, Number: pr.Number, Title: title, Body: description, Author: as, URL: pr.HTMLURL}, branch, nil
}

// Propose writes one file on a new branch, as person, and opens a pull
// request for it. Nothing reaches main, and so the cluster, until a person
// allowed to merges it in Gitea; the gateway cannot merge.
// branch names the new branch (empty: change/<as>-<time>); note, if set,
// ends the pull request's description (whom it was asked for).
func (g *Gitea) Propose(ctx context.Context, as, org, repo, path, content, title, description, branch, note string) (Change, error) {
	if err := validOrg(org); err != nil {
		return Change{}, err
	}
	if err := validRepo(repo); err != nil {
		return Change{}, err
	}
	if err := validPath(path); err != nil || path == "" {
		return Change{}, fmt.Errorf("%w: path %q", ErrInvalid, path)
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
	file := fmt.Sprintf("/repos/%s/%s/contents/%s", org, repo, escapePath(path))
	req := map[string]any{
		"branch":     "main",
		"new_branch": branch,
		"message":    title,
		"content":    base64.StdEncoding.EncodeToString([]byte(content)),
	}
	var existing struct {
		SHA string `json:"sha"`
	}
	method := http.MethodPost
	if status, err := g.do(ctx, as, http.MethodGet, file, nil, &existing); err == nil && existing.SHA != "" {
		req["sha"] = existing.SHA
		method = http.MethodPut
	} else if err != nil && status != http.StatusNotFound {
		return Change{}, err
	}
	if _, err := g.do(ctx, as, method, file, req, nil); err != nil {
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
	return Change{Namespace: org, Repo: repo, Number: pr.Number, Title: title, Body: description, Author: as, URL: pr.HTMLURL}, nil
}

// Changes lists the namespace's open pull requests, with their diffs.
func (g *Gitea) Changes(ctx context.Context, as, org string) ([]Change, error) {
	repos, err := g.Repos(ctx, as, org)
	if err != nil {
		return nil, err
	}
	var out []Change
	for _, r := range repos {
		if r.Empty {
			continue
		}
		var prs []struct {
			Number  int    `json:"number"`
			Title   string `json:"title"`
			Body    string `json:"body"`
			HTMLURL string `json:"html_url"`
			User    struct {
				Login string `json:"login"`
			} `json:"user"`
		}
		if _, err := g.do(ctx, as, http.MethodGet, fmt.Sprintf("/repos/%s/%s/pulls?state=open&limit=50", org, r.Name), nil, &prs); err != nil {
			return nil, err
		}
		for _, p := range prs {
			c := Change{Namespace: org, Repo: r.Name, Number: p.Number, Title: p.Title, Body: p.Body, Author: p.User.Login, URL: p.HTMLURL}
			g.do(ctx, as, http.MethodGet, fmt.Sprintf("/repos/%s/%s/pulls/%d.diff", org, r.Name, p.Number), nil, &c.Diff)
			out = append(out, c)
		}
	}
	return out, nil
}

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

// CreateNamespace makes a namespace, a Gitea organisation, as person. Gitea
// lets only its admins, the root admins, make organisations. Its creator is
// its first owner; the members sync then makes its config repo, with them as
// its first admin, and the Open WebUI sync its channel.
func (g *Gitea) CreateNamespace(ctx context.Context, as, name, description string) (string, error) {
	if err := validOrg(name); err != nil {
		return "", err
	}
	if len(name) > 40 || strings.ToLower(name) != name || strings.ContainsAny(name, "._") {
		return "", fmt.Errorf("%w: a namespace's name is lowercase letters, digits and dashes, at most 40: %q", ErrInvalid, name)
	}
	status, err := g.do(ctx, as, http.MethodPost, "/orgs", map[string]any{
		"username": name, "description": description, "visibility": "limited",
	}, nil)
	switch {
	case status == http.StatusUnprocessableEntity || status == http.StatusConflict:
		return "", fmt.Errorf("%w: there is a namespace or user %q already", ErrInvalid, name)
	case errors.Is(err, ErrForbidden):
		return "", refused{as + " may not make namespaces: only root admins may"}
	case err != nil:
		return "", err
	}
	return name, nil
}

// CreateRepo makes an empty repo in a namespace, as person: Gitea lets the
// namespace's admins and the root admins. It starts with a README on main,
// so changes can be proposed to it; nothing in it runs until a stack in the
// namespace's config names it as its source.
func (g *Gitea) CreateRepo(ctx context.Context, as, org, repo, description string, private bool, template string) (string, error) {
	if err := validOrg(org); err != nil {
		return "", err
	}
	files := templates.AppFiles(template, org, repo)
	if template != "" && files == nil {
		return "", fmt.Errorf("%w: there is no template %q; there are: %s", ErrInvalid, template, strings.Join(templates.Apps(), ", "))
	}
	if err := validRepo(repo); err != nil {
		return "", err
	}
	if repo == ConfigRepo {
		return "", fmt.Errorf("%w: config is made with the namespace", ErrInvalid)
	}
	if strings.ToLower(repo) != repo || len(repo) > 40 || strings.ContainsAny(repo, "._") {
		return "", fmt.Errorf("%w: a repo's name is lowercase letters, digits and dashes, at most 40, as it may become a hostname: %q", ErrInvalid, repo)
	}
	var r struct {
		HTMLURL string `json:"html_url"`
	}
	status, err := g.do(ctx, as, http.MethodPost, "/orgs/"+org+"/repos", map[string]any{
		"name": repo, "description": description, "private": private,
		"auto_init": true, "default_branch": "main", "readme": "Default",
	}, &r)
	switch {
	case status == http.StatusConflict || status == http.StatusUnprocessableEntity:
		return "", fmt.Errorf("%w: %s/%s exists already", ErrInvalid, org, repo)
	case status == http.StatusNotFound:
		return "", fmt.Errorf("%w: there is no namespace %s", ErrInvalid, org)
	case errors.Is(err, ErrForbidden):
		return "", refused{fmt.Sprintf("%s may not make repos in %s: its admins and root admins may", as, org)}
	case err != nil:
		return "", err
	}
	if files != nil {
		raw := map[string][]byte{}
		for p, c := range files {
			raw[p] = []byte(c)
		}
		ops, err := g.fileOps(ctx, org, repo, raw)
		if err != nil {
			return "", err
		}
		for _, op := range ops {
			op["content"] = base64.StdEncoding.EncodeToString([]byte(op["content"]))
		}
		// As the person, who may write to the repo they just made.
		if _, err := g.do(ctx, as, http.MethodPost, fmt.Sprintf("/repos/%s/%s/contents", org, repo), map[string]any{
			"branch": "main", "message": "Start from the template " + template, "files": ops,
		}, nil); err != nil {
			return "", fmt.Errorf("the repo %s/%s is made, but its template's files are not: %w", org, repo, err)
		}
	}
	return r.HTMLURL, nil
}
