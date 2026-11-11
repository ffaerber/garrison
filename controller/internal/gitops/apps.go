package gitops

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"homecloud/controller/internal/apply"
	"homecloud/controller/internal/hostnames"
	"homecloud/controller/internal/render"
	"homecloud/controller/internal/stack"
)

// App is one deployed environment of an app: a repo with a stack.yaml in a
// namespace other than system, deployed from the branch named after the
// environment.
type App struct {
	Namespace   string `json:"namespace"`
	Repo        string `json:"repo"`
	Environment string `json:"environment"`
	Stack       string `json:"stack"` // Swarm stack name: the environment's hostname
	Commit      string `json:"commit"`
}

func appKey(ns, repo, env string) string { return ns + "/" + repo + "@" + env }

func (c *Config) claimsFile() string {
	if c.ClaimsFile != "" {
		return c.ClaimsFile
	}
	return filepath.Join(c.WorkDir, "claims.json")
}

// get calls the Gitea API and decodes the answer.
func (c *Config) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.giteaURL()+"/api/v1"+path, nil)
	if err != nil {
		return err
	}
	req.SetBasicAuth(c.User, c.password)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("gitea GET %s: %s", path, resp.Status)
	}
	return decodeJSON(resp, out)
}

func (c *Config) namespaces(ctx context.Context) ([]string, error) {
	var out []string
	for page := 1; page < 20; page++ {
		var batch []struct {
			Name string `json:"username"`
		}
		if err := c.get(ctx, fmt.Sprintf("/orgs?limit=50&page=%d", page), &batch); err != nil {
			return nil, err
		}
		for _, o := range batch {
			if o.Name != c.Org && stackName.MatchString(o.Name) {
				out = append(out, o.Name)
			}
		}
		if len(batch) < 50 {
			break
		}
	}
	slices.Sort(out)
	return out, nil
}

func (c *Config) branches(ctx context.Context, org, repo string) ([]string, error) {
	var batch []struct {
		Name string `json:"name"`
	}
	if err := c.get(ctx, fmt.Sprintf("/repos/%s/%s/branches?limit=50", url.PathEscape(org), url.PathEscape(repo)), &batch); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(batch))
	for _, b := range batch {
		out = append(out, b.Name)
	}
	return out, nil
}

// checkout brings a branch of a repo to its tip in dir and returns the commit.
func (c *Config) checkout(ctx context.Context, org, repo, branch, dir string) (string, error) {
	remote := fmt.Sprintf("%s/%s/%s.git", c.giteaURL(), org, repo)
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		if err := os.RemoveAll(dir); err != nil {
			return "", err
		}
		if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
			return "", err
		}
		if _, err := c.git(ctx, "clone", "-q", "--branch", branch, remote, dir); err != nil {
			return "", err
		}
	} else {
		for _, args := range [][]string{
			{"-C", dir, "fetch", "-q", remote, branch},
			{"-C", dir, "reset", "-q", "--hard", "FETCH_HEAD"},
			{"-C", dir, "clean", "-qfdx"},
		} {
			if _, err := c.git(ctx, args...); err != nil {
				return "", err
			}
		}
	}
	return c.git(ctx, "-C", dir, "rev-parse", "HEAD")
}

func readStack(dir string) (*stack.Stack, error) {
	data, err := os.ReadFile(filepath.Join(dir, "stack.yaml"))
	if err != nil {
		return nil, err
	}
	return stack.Parse(data)
}

// reserved reads root's reserved hostnames from the platform repo's checkout.
func (c *Config) reserved() []string {
	data, err := os.ReadFile(filepath.Join(c.WorkDir, "repos", "platform", "reserved-hostnames.yaml"))
	if err != nil {
		return nil
	}
	var doc struct {
		Reserved []string `yaml:"reserved"`
	}
	yaml.Unmarshal(data, &doc)
	return doc.Reserved
}

// syncApps deploys every environment of every app, removes environments
// whose branch or repo is gone, and keeps hostname claims. It returns the
// apps now deployed. An app that cannot be read or deployed keeps running
// at its previous commit, if it had one, and its error is reported.
func (c *Config) syncApps(ctx context.Context, force bool, prev map[string]App) (map[string]App, []error) {
	next := map[string]App{}
	keep := func(key string) {
		if a, ok := prev[key]; ok {
			next[key] = a
		}
	}
	keepApp := func(ns, repo string) {
		for k, a := range prev {
			if a.Namespace == ns && a.Repo == repo {
				next[k] = a
			}
		}
	}
	keepNamespace := func(ns string) {
		for k, a := range prev {
			if a.Namespace == ns {
				next[k] = a
			}
		}
	}

	namespaces, err := c.namespaces(ctx)
	if err != nil {
		// Without the list, nothing may be taken down.
		return prev, []error{fmt.Errorf("list namespaces: %w", err)}
	}
	// Apps pull their images from the registry with the controller's
	// credentials. Without them, nothing is deployed; signing in is tried again
	// on every sync, since after a boot Gitea may still be starting.
	if !c.signedIn {
		if err := c.registryLogin(ctx); err != nil {
			return prev, []error{fmt.Errorf("sign in to the registry, needed to deploy apps: %w", err)}
		}
		c.signedIn = true
	}
	reg := hostnames.NewRegistry(c.reserved())
	if err := reg.Load(c.claimsFile()); err != nil {
		return prev, []error{err}
	}
	var errs []error
	fail := func(format string, a ...any) {
		err := fmt.Errorf(format, a...)
		log.Printf("apps: %v", err)
		errs = append(errs, err)
	}

	for _, ns := range namespaces {
		repos, err := c.reposOf(ctx, ns)
		if err != nil {
			fail("list repos of %s: %v", ns, err)
			keepNamespace(ns)
			continue
		}
		for _, r := range repos {
			base := filepath.Join(c.WorkDir, "apps", ns, r.Name)
			mainDir := filepath.Join(base, "@"+r.DefaultBranch)
			if _, err := c.checkout(ctx, ns, r.Name, r.DefaultBranch, mainDir); err != nil {
				fail("fetch %s/%s: %v", ns, r.Name, err)
				keepApp(ns, r.Name)
				continue
			}
			if _, err := os.Stat(filepath.Join(mainDir, "stack.yaml")); err != nil {
				continue // not an app
			}
			s, err := readStack(mainDir)
			if err != nil {
				fail("%s/%s: stack.yaml: %v", ns, r.Name, err)
				keepApp(ns, r.Name)
				continue
			}
			if s.Name != r.Name || s.Namespace != ns {
				fail("%s/%s: stack.yaml must say stack: %s and namespace: %s", ns, r.Name, r.Name, ns)
				keepApp(ns, r.Name)
				continue
			}
			if err := reg.Claim(s); err != nil {
				fail("%s/%s: not deployed: %v", ns, r.Name, err)
				keepApp(ns, r.Name)
				continue
			}
			branches, err := c.branches(ctx, ns, r.Name)
			if err != nil {
				fail("branches of %s/%s: %v", ns, r.Name, err)
				keepApp(ns, r.Name)
				continue
			}
			for _, env := range s.EnvironmentNames() {
				key := appKey(ns, r.Name, env)
				if !slices.Contains(branches, env) {
					continue // no branch, no environment
				}
				dir := mainDir
				if env != r.DefaultBranch {
					dir = filepath.Join(base, "@"+env)
				}
				commit, err := c.checkout(ctx, ns, r.Name, env, dir)
				if err != nil {
					fail("fetch %s branch %s: %v", key, env, err)
					keep(key)
					continue
				}
				es, err := readStack(dir)
				if err == nil && (es.Name != s.Name || es.Namespace != ns) {
					err = errors.New("stack.yaml names another stack or namespace than on " + r.DefaultBranch)
				}
				if err == nil {
					if _, ok := es.Environments[env]; !ok {
						err = fmt.Errorf("stack.yaml on %s has no environment %s", env, env)
					}
				}
				if err != nil {
					fail("%s: %v", key, err)
					keep(key)
					continue
				}
				app := App{Namespace: ns, Repo: r.Name, Environment: env, Stack: render.StackName(es, env), Commit: commit}
				if old, ok := prev[key]; ok && !force && old.Commit == commit {
					next[key] = old
					continue
				}
				// An image the pipeline builds for this commit must exist
				// first; a failed pipeline leaves the app where it was.
				if missing := c.missingImages(ctx, es, commit); len(missing) > 0 {
					if c.waiting[key] != commit {
						log.Printf("apps: %s at %.12s waits for %s", key, commit, strings.Join(missing, ", "))
						c.waiting[key] = commit
					}
					keep(key)
					continue
				}
				if err := apply.Stack(ctx, c.Run, es, render.Options{Environment: env, Domain: c.Domain, Commit: commit}); err != nil {
					fail("deploy %s: %v", key, err)
					keep(key)
					continue
				}
				next[key] = app
				log.Printf("apps: deployed %s at %.12s as %s", key, commit, app.Stack)
			}
		}
	}

	// Environments no longer wanted are taken down; an app with none left
	// gives up its hostnames.
	for key, a := range prev {
		if _, ok := next[key]; ok {
			continue
		}
		if _, err := c.Run(ctx, nil, nil, "docker", "stack", "rm", a.Stack); err != nil {
			fail("remove %s: %v", key, err)
			next[key] = a
			continue
		}
		log.Printf("apps: removed %s (%s)", key, a.Stack)
	}
	for _, a := range prev {
		still := false
		for _, n := range next {
			still = still || (n.Namespace == a.Namespace && n.Repo == a.Repo)
		}
		if !still && !c.appDeclared(namespaces, a.Namespace, a.Repo) {
			reg.Release(a.Namespace, a.Repo)
		}
	}
	if err := reg.Save(c.claimsFile()); err != nil {
		errs = append(errs, err)
	}
	return next, errs
}

// appDeclared reports whether the app's main checkout still has a stack.yaml,
// so an app with no deployed environment keeps its hostnames.
func (c *Config) appDeclared(namespaces []string, ns, repo string) bool {
	if !slices.Contains(namespaces, ns) {
		return false
	}
	matches, _ := filepath.Glob(filepath.Join(c.WorkDir, "apps", ns, repo, "@*", "stack.yaml"))
	return len(matches) > 0 && c.repoExists(ns, repo)
}

func (c *Config) repoExists(ns, repo string) bool {
	var r struct {
		Name string `json:"name"`
	}
	return c.get(context.Background(), fmt.Sprintf("/repos/%s/%s", url.PathEscape(ns), url.PathEscape(repo)), &r) == nil
}

// AppsOf lists the deployed apps of a namespace, for the control plane.
func (s State) AppsOf(namespace string) []App {
	var out []App
	for _, a := range s.Apps {
		if a.Namespace == namespace {
			out = append(out, a)
		}
	}
	slices.SortFunc(out, func(a, b App) int { return strings.Compare(a.Stack, b.Stack) })
	return out
}

// missingImages lists the images of a stack, from the installation's
// registry, that do not exist there yet.
func (c *Config) missingImages(ctx context.Context, s *stack.Stack, commit string) []string {
	prefix := render.Registry(c.Domain) + "/"
	var missing []string
	for _, name := range s.ServiceNames() {
		image := render.Image(s.Services[name].Image, c.Domain, commit)
		ref, ok := strings.CutPrefix(image, prefix)
		if !ok {
			continue
		}
		path, tag, ok := strings.Cut(ref, ":")
		owner, pkg, ok2 := strings.Cut(path, "/")
		if !ok || !ok2 {
			missing = append(missing, image+" (needs <registry>/<namespace>/<name>:<tag>)")
			continue
		}
		var out struct{}
		if err := c.get(ctx, fmt.Sprintf("/packages/%s/container/%s/%s", url.PathEscape(owner), url.PathEscape(pkg), url.PathEscape(tag)), &out); err != nil {
			missing = append(missing, image)
		}
	}
	return missing
}

// registryLogin signs the controller's Docker client in to the installation's
// registry, so deploys can hand Swarm the credentials to pull app images.
func (c *Config) registryLogin(ctx context.Context) error {
	_, err := c.Run(ctx, nil, []byte(c.password), "docker", "login", render.Registry(c.Domain), "--username", c.User, "--password-stdin")
	return err
}
