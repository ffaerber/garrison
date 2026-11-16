package gateway

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"slices"
)

// What the dashboard's secrets form needs of Gitea (secrets.Gitea), as the
// platform's own account.

// IsAdmin tells whether person is a root admin.
func (g *Gitea) IsAdmin(ctx context.Context, person string) (bool, error) {
	if !userName.MatchString(person) {
		return false, fmt.Errorf("%w: user %q", ErrInvalid, person)
	}
	var u struct {
		IsAdmin bool `json:"is_admin"`
	}
	status, err := g.do(ctx, "", http.MethodGet, "/users/"+person, nil, &u)
	if status == http.StatusNotFound {
		return false, nil
	}
	return u.IsAdmin, err
}

// Orgs are the names of the organisations person sees.
func (g *Gitea) Orgs(ctx context.Context, person string) ([]string, error) {
	nss, err := g.Namespaces(ctx, person)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(nss))
	for _, ns := range nss {
		out = append(out, ns.Name)
	}
	return out, nil
}

// Files are the names of the files in a directory of a repo's main branch;
// none if it does not exist.
func (g *Gitea) Files(ctx context.Context, org, repo, dir string) ([]string, error) {
	if err := validOrg(org); err != nil {
		return nil, err
	}
	if err := validRepo(repo); err != nil {
		return nil, err
	}
	if err := validPath(dir); err != nil || dir == "" {
		return nil, fmt.Errorf("%w: path %q", ErrInvalid, dir)
	}
	var entries []Entry
	status, err := g.do(ctx, "", http.MethodGet, fmt.Sprintf("/repos/%s/%s/contents/%s?ref=main", org, repo, escapePath(dir)), nil, &entries)
	if status == http.StatusNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.Type == "file" {
			out = append(out, e.Name)
		}
	}
	slices.Sort(out)
	return out, nil
}

// File is a file of a repo's main branch; "" if it does not exist.
func (g *Gitea) File(ctx context.Context, org, repo, path string) (string, error) {
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
	status, err := g.do(ctx, "", http.MethodGet, fmt.Sprintf("/repos/%s/%s/raw/%s?ref=main", org, repo, escapePath(path)), nil, &out)
	if status == http.StatusNotFound {
		return "", nil
	}
	return out, err
}

// Commit writes files (nil: deletes) to a repo's main branch in one commit
// by the platform's account, naming author as its author. On config, whose
// main takes only approved pull requests, only the platform's account may
// push (see protect): the dashboard writes secrets there for their admins.
func (g *Gitea) Commit(ctx context.Context, org, repo, message, author string, files map[string][]byte) error {
	if err := validOrg(org); err != nil {
		return err
	}
	if err := validRepo(repo); err != nil {
		return err
	}
	if !userName.MatchString(author) {
		return fmt.Errorf("%w: user %q", ErrInvalid, author)
	}
	var ops []map[string]string
	for _, path := range sortedKeys(files) {
		if err := validPath(path); err != nil || path == "" {
			return fmt.Errorf("%w: path %q", ErrInvalid, path)
		}
		var have struct {
			SHA string `json:"sha"`
		}
		status, err := g.do(ctx, "", http.MethodGet, fmt.Sprintf("/repos/%s/%s/contents/%s?ref=main", org, repo, escapePath(path)), nil, &have)
		if err != nil && status != http.StatusNotFound {
			return err
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
			op["content"] = base64.StdEncoding.EncodeToString(files[path])
		}
		ops = append(ops, op)
	}
	if len(ops) == 0 {
		return nil
	}
	_, err := g.do(ctx, "", http.MethodPost, fmt.Sprintf("/repos/%s/%s/contents", org, repo), map[string]any{
		"branch": "main", "message": message, "files": ops,
		"author": map[string]string{"name": author, "email": author + "@users.noreply.garrison"},
	}, nil)
	return err
}
