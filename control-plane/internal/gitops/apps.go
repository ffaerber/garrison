package gitops

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"homecloud/control-plane/internal/apply"
	"homecloud/control-plane/internal/hostnames"
	"homecloud/control-plane/internal/render"
	"homecloud/control-plane/internal/stack"
)

// App is one deployed environment of an app: a stack of a namespace other
// than system, stacks/<name>.yaml in its config repo, deployed from the
// branch of its source repo named after the environment, or as config says.
type App struct {
	Namespace   string `json:"namespace"`
	Repo        string `json:"repo"` // the stack's name: config/stacks/<repo>.yaml
	Environment string `json:"environment"`
	Stack       string `json:"stack"`  // Swarm stack name: the environment's hostname
	Commit      string `json:"commit"` // of the source repo's branch; of config without a source
	Source      string `json:"source,omitempty"`
	Spec        string `json:"spec,omitempty"` // a hash of the stack file, which redeploys when it changes
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

// MaxPreviews is how many pull requests of a repo get a preview at once:
// the newest.
const MaxPreviews = 3

// previews are the open pull requests of a repo that get a preview, by
// number, with their newest commit: the newest, from branches of the repo
// itself.
func (c *Config) previews(ctx context.Context, org, repo string) (map[int]string, error) {
	var prs []struct {
		Number int `json:"number"`
		Head   struct {
			Ref  string `json:"ref"`
			SHA  string `json:"sha"`
			Repo struct {
				FullName string `json:"full_name"`
			} `json:"repo"`
		} `json:"head"`
	}
	if err := c.get(ctx, fmt.Sprintf("/repos/%s/%s/pulls?state=open&sort=newest&limit=50", url.PathEscape(org), url.PathEscape(repo)), &prs); err != nil {
		return nil, err
	}
	slices.SortFunc(prs, func(a, b struct {
		Number int `json:"number"`
		Head   struct {
			Ref  string `json:"ref"`
			SHA  string `json:"sha"`
			Repo struct {
				FullName string `json:"full_name"`
			} `json:"repo"`
		} `json:"head"`
	}) int {
		return b.Number - a.Number
	})
	out := map[int]string{}
	for _, p := range prs {
		if len(out) == MaxPreviews {
			break
		}
		if p.Head.Repo.FullName == org+"/"+repo && p.Head.SHA != "" {
			out[p.Number] = p.Head.SHA
		}
	}
	return out, nil
}

// heads are a repo's branches with their newest commits.
func (c *Config) heads(ctx context.Context, org, repo string) (map[string]string, error) {
	var batch []struct {
		Name   string `json:"name"`
		Commit struct {
			ID string `json:"id"`
		} `json:"commit"`
	}
	if err := c.get(ctx, fmt.Sprintf("/repos/%s/%s/branches?limit=50", url.PathEscape(org), url.PathEscape(repo)), &batch); err != nil {
		return nil, err
	}
	out := make(map[string]string, len(batch))
	for _, b := range batch {
		out[b.Name] = b.Commit.ID
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
	// Apps pull their images from the registry with the deployer's
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

	declared := map[string]bool{} // ns/name of every stack in a config repo
	for _, ns := range namespaces {
		cfgDir := filepath.Join(c.WorkDir, "apps", ns, "config", "@main")
		cfgCommit, err := c.checkout(ctx, ns, "config", "main", cfgDir)
		if err != nil {
			fail("fetch %s/config: %v", ns, err)
			keepNamespace(ns)
			continue
		}
		files, _ := filepath.Glob(filepath.Join(cfgDir, stack.Dir, "*.yaml"))
		parsed := map[string]*stack.Stack{} // the namespace's stacks that could be read, by name
		for _, file := range files {
			name := strings.TrimSuffix(filepath.Base(file), ".yaml")
			declared[ns+"/"+name] = true
			data, err := os.ReadFile(file)
			var s *stack.Stack
			if err == nil {
				s, err = stack.ParseIn(data, ns, name)
			}
			if err != nil {
				fail("%s/config/%s/%s.yaml: %v", ns, stack.Dir, name, err)
				keepApp(ns, name)
				continue
			}
			sum := sha256.Sum256(data)
			spec := hex.EncodeToString(sum[:8])
			// Environments and the commit each is deployed at: the source
			// repo's branches named in the stack, and a preview of each of
			// its open pull requests; without a source, main as config says.
			from := map[string]string{}
			var previews []string
			if s.Source == "" {
				from[stack.MainEnvironment] = cfgCommit
			} else {
				heads, err := c.heads(ctx, ns, s.Source)
				if err != nil {
					fail("%s/%s: its source %s/%s: %v", ns, name, ns, s.Source, err)
					keepApp(ns, name)
					continue
				}
				for _, env := range s.Branches() {
					if sha, ok := heads[env]; ok {
						from[env] = sha
					}
				}
				if _, ok := s.Environments[stack.PreviewEnvironment]; ok {
					prs, err := c.previews(ctx, ns, s.Source)
					if err != nil {
						fail("pull requests of %s/%s: %v", ns, s.Source, err)
						keepApp(ns, name)
						continue
					}
					for n, sha := range prs {
						from[stack.PreviewName(n)] = sha
						previews = append(previews, stack.PreviewName(n))
					}
				}
			}
			s.WithPreviews(previews)
			if err := reg.Claim(s); err != nil {
				fail("%s/%s: not deployed: %v", ns, name, err)
				keepApp(ns, name)
				continue
			}
			for _, env := range s.EnvironmentNames() {
				key := appKey(ns, name, env)
				commit, ok := from[env]
				if !ok {
					continue // no branch, no environment
				}
				app := App{Namespace: ns, Repo: name, Environment: env, Stack: render.StackName(s, env), Commit: commit, Source: s.Source, Spec: spec}
				if old, ok := prev[key]; ok && !force && old.Spec == spec && (s.Source == "" || old.Commit == commit) {
					next[key] = old
					continue
				}
				// An image the pipeline builds for this commit must exist
				// first; a failed pipeline leaves the app where it was.
				if missing := c.missingImages(ctx, s, commit); len(missing) > 0 {
					if c.waiting[key] != commit {
						log.Printf("apps: %s at %.12s waits for %s", key, commit, strings.Join(missing, ", "))
						c.waiting[key] = commit
					}
					keep(key)
					continue
				}
				// A redeploy may migrate or break the data: the deployer
				// snapshots it first, and does not deploy if it cannot.
				if _, ok := prev[key]; ok && !stack.IsPreview(env) {
					snap := apply.DeploySnapshot + apply.SnapshotStamp(time.Now()) + "-" + commit[:min(7, len(commit))]
					took, err := apply.SnapshotEnv(ctx, c.Run, "", s, env, snap)
					if err != nil {
						fail("deploy %s: no snapshot before it, so not deployed: %v", key, err)
						keep(key)
						continue
					}
					c.snapsChanged = c.snapsChanged || took
				}
				if err := apply.Stack(ctx, c.Run, s, render.Options{Environment: env, Domain: c.Domain, Commit: commit}); err != nil {
					fail("deploy %s: %v", key, err)
					keep(key)
					continue
				}
				next[key] = app
				log.Printf("apps: deployed %s at %.12s as %s", key, commit, app.Stack)
			}
			parsed[name] = s
		}
		for _, err := range c.restores(ctx, ns, cfgDir, parsed, next) {
			fail("%v", err)
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
		if !still && !declared[a.Namespace+"/"+a.Repo] {
			reg.Release(a.Namespace, a.Repo)
		}
	}
	if err := reg.Save(c.claimsFile()); err != nil {
		errs = append(errs, err)
	}
	return next, errs
}

// AppsOf lists the deployed apps of a namespace, for the gateway.
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

// registryLogin signs the deployer's Docker client in to the installation's
// registry, so deploys can hand Swarm the credentials to pull app images.
func (c *Config) registryLogin(ctx context.Context) error {
	_, err := c.Run(ctx, nil, []byte(c.password), "docker", "login", render.Registry(c.Domain), "--username", c.User, "--password-stdin")
	return err
}
