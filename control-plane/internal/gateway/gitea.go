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
)

// Gitea calls Gitea's API as its admin account, acting as a given person
// through the Sudo header. Gitea then checks that person's own rights, so
// the gateway and its agent can never do more than the person could.
type Gitea struct {
	URL      string // e.g. http://gitea_gitea:3000
	User     string // admin account
	Password string
	HTTP     *http.Client
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

// Namespaces lists the organisations person can see.
func (g *Gitea) Namespaces(ctx context.Context, as string) ([]Namespace, error) {
	var raw []struct {
		Name        string `json:"username"`
		Description string `json:"description"`
	}
	if _, err := g.do(ctx, as, http.MethodGet, "/orgs?limit=50", nil, &raw); err != nil {
		return nil, err
	}
	out := make([]Namespace, 0, len(raw))
	for _, o := range raw {
		out = append(out, Namespace{Name: o.Name, Description: o.Description})
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
	if _, err := g.do(ctx, as, http.MethodGet, fmt.Sprintf("/repos/%s/%s", org, repo), nil, &r); err != nil {
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
func (g *Gitea) Propose(ctx context.Context, as, org, repo, path, content, title, description string) (Change, error) {
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
	branch := fmt.Sprintf("change/%s-%d", as, time.Now().UnixNano()/1e6)
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
