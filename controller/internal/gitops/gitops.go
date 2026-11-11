// Package gitops keeps the cluster in line with a namespace in Gitea. A
// namespace is a Gitea organisation; every repo in it that holds a
// compose.yaml on its default branch is one stack, named after the repo.
package gitops

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"time"

	"homecloud/controller/internal/apply"
	"homecloud/controller/internal/system"
)

// LastStack is deployed after every other stack, because deploying it
// replaces the controller doing the deploying.
const LastStack = "controller"

// A repo name that can be a Swarm stack name.
var stackName = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

type Config struct {
	Domain       string   // installation domain; Gitea is git.<domain>
	GiteaURL     string   // where to reach Gitea; https://git.<domain> if empty
	User         string   // Gitea account the controller reads repos with
	PasswordFile string   // its password
	CAFile       string   // certificate the installation's TLS is verified with
	Org          string   // namespace to deploy, e.g. system
	WorkDir      string   // controller's own directory: checkouts and state
	Env          []string // KEY=VALUE passed to every deploy
	Run          apply.Runner
	HTTP         *http.Client // for the Gitea API; built from the fields above if nil
	Dashboards   Dashboards   // optional: keeps Grafana's dashboards as they are in Git
	Apps         bool         // also deploy apps: stack.yaml repos of every other namespace
	ClaimsFile   string       // hostname claims; WorkDir/claims.json if empty

	password string
	home     string            // HOME for git, holding the .netrc
	waiting  map[string]string // app environment -> commit whose image is awaited, to log it once
	signedIn bool              // the Docker client is signed in to the registry
}

// Dashboards syncs the dashboards/*.json of a namespace's repos, given as
// repo name to checkout, to Grafana.
type Dashboards interface {
	Sync(ctx context.Context, namespace string, repos map[string]string) error
}

// State is what the controller last applied, kept in WorkDir/applied.json:
// the commit of each stack's repo.
type State struct {
	Stacks  map[string]string `json:"stacks"`
	Apps    map[string]App    `json:"apps,omitempty"` // namespace/repo@environment -> deployed app
	Applied time.Time         `json:"applied"`
}

// Names lists the applied stacks in name order.
func (s State) Names() []string {
	names := make([]string, 0, len(s.Stacks))
	for n := range s.Stacks {
		names = append(names, n)
	}
	slices.Sort(names)
	return names
}

func ReadState(workDir string) (State, error) {
	s := State{Stacks: map[string]string{}, Apps: map[string]App{}}
	data, err := os.ReadFile(filepath.Join(workDir, "applied.json"))
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	if err := json.Unmarshal(data, &s); err != nil {
		return s, err
	}
	if s.Stacks == nil {
		s.Stacks = map[string]string{}
	}
	if s.Apps == nil {
		s.Apps = map[string]App{}
	}
	return s, nil
}

func (c *Config) writeState(s State) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	file := filepath.Join(c.WorkDir, "applied.json")
	if err := os.WriteFile(file+".tmp", append(data, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(file+".tmp", file)
}

func (c *Config) giteaURL() string {
	if c.GiteaURL != "" {
		return strings.TrimSuffix(c.GiteaURL, "/")
	}
	return "https://git." + c.Domain
}

// public is true when Gitea is reached at its public name through Traefik on
// this machine, as the controller CLI on the host does.
func (c *Config) public() bool { return c.giteaURL() == "https://git."+c.Domain }

// lock keeps two controllers, say the CLI on the host and the service, from
// applying at the same time. The kernel drops it when the process dies.
func (c *Config) lock() (func(), error) {
	f, err := os.OpenFile(filepath.Join(c.WorkDir, "lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() { syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }, nil
}

// prepare reads the password, writes the netrc git reads it from (so it never
// appears on a command line) and sets up the API client.
func (c *Config) prepare() error {
	if err := os.MkdirAll(filepath.Join(c.WorkDir, "repos"), 0o700); err != nil {
		return err
	}
	pw, err := os.ReadFile(c.PasswordFile)
	if err != nil {
		return err
	}
	c.password = strings.TrimSpace(string(pw))
	u, err := url.Parse(c.giteaURL())
	if err != nil {
		return err
	}
	// One home per Gitea address: the CLI on the host and the service in Swarm
	// share WorkDir but reach Gitea under different names.
	c.home = filepath.Join(c.WorkDir, "home", u.Hostname())
	if err := os.MkdirAll(c.home, 0o700); err != nil {
		return err
	}
	netrc := fmt.Sprintf("machine %s login %s password %s\n", u.Hostname(), c.User, c.password)
	if err := os.WriteFile(filepath.Join(c.home, ".netrc"), []byte(netrc), 0o600); err != nil {
		return err
	}
	c.waiting = map[string]string{}
	if c.HTTP == nil {
		c.HTTP = &http.Client{Timeout: 30 * time.Second}
		if c.public() {
			tr, err := c.publicTransport()
			if err != nil {
				return err
			}
			c.HTTP.Transport = tr
		}
	}
	return nil
}

// publicTransport verifies the installation's certificate and sends
// git.<domain> to Traefik on this machine.
func (c *Config) publicTransport() (*http.Transport, error) {
	pem, err := os.ReadFile(c.CAFile)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("%s: no certificate", c.CAFile)
	}
	var d net.Dialer
	return &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: pool},
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return d.DialContext(ctx, network, "127.0.0.1:443")
		},
	}, nil
}

type repo struct {
	Name          string `json:"name"`
	DefaultBranch string `json:"default_branch"`
	Empty         bool   `json:"empty"`
	Archived      bool   `json:"archived"`
}

// repos lists the system organisation's repos that can hold a stack, with
// the controller last.
func (c *Config) repos(ctx context.Context) ([]repo, error) {
	out, err := c.reposOf(ctx, c.Org)
	if err != nil {
		return nil, err
	}
	slices.SortFunc(out, func(a, b repo) int {
		switch {
		case a.Name == LastStack:
			return 1
		case b.Name == LastStack:
			return -1
		}
		return strings.Compare(a.Name, b.Name)
	})
	return out, nil
}

// reposOf lists an organisation's repos that can hold a stack, by name.
func (c *Config) reposOf(ctx context.Context, org string) ([]repo, error) {
	var out []repo
	for page := 1; ; page++ {
		u := fmt.Sprintf("%s/api/v1/orgs/%s/repos?limit=50&page=%d", c.giteaURL(), url.PathEscape(org), page)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, err
		}
		req.SetBasicAuth(c.User, c.password)
		resp, err := c.HTTP.Do(req)
		if err != nil {
			return nil, err
		}
		var batch []repo
		err = json.NewDecoder(resp.Body).Decode(&batch)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("list repos of %s: %s", org, resp.Status)
		}
		if err != nil {
			return nil, fmt.Errorf("list repos of %s: %w", org, err)
		}
		for _, r := range batch {
			if !r.Empty && !r.Archived && stackName.MatchString(r.Name) {
				out = append(out, r)
			}
		}
		if len(batch) < 50 {
			break
		}
	}
	slices.SortFunc(out, func(a, b repo) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

func decodeJSON(resp *http.Response, out any) error { return json.NewDecoder(resp.Body).Decode(out) }

func (c *Config) git(ctx context.Context, args ...string) (string, error) {
	env := []string{
		"HOME=" + c.home, // git finds .netrc here
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_TERMINAL_PROMPT=0",
	}
	var base []string
	if c.public() {
		base = []string{
			"-c", "http.sslCAInfo=" + c.CAFile,
			"-c", fmt.Sprintf("http.curloptResolve=git.%s:443:127.0.0.1", c.Domain),
		}
	}
	out, err := c.Run(ctx, env, nil, "git", append(base, args...)...)
	return strings.TrimSpace(string(out)), err
}

// fetch brings a repo's checkout to the tip of its default branch and
// returns the commit.
func (c *Config) fetch(ctx context.Context, r repo) (string, error) {
	dir := filepath.Join(c.WorkDir, "repos", r.Name)
	remote := fmt.Sprintf("%s/%s/%s.git", c.giteaURL(), c.Org, r.Name)
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		if err := os.RemoveAll(dir); err != nil {
			return "", err
		}
		if _, err := c.git(ctx, "clone", "-q", "--branch", r.DefaultBranch, remote, dir); err != nil {
			return "", err
		}
	} else {
		for _, args := range [][]string{
			{"-C", dir, "fetch", "-q", remote, r.DefaultBranch},
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

// Sync deploys every stack whose repo moved since it was last applied, or
// every stack if force is set, and takes down stacks whose repo is gone or no
// longer holds a compose.yaml. What succeeded is recorded; what failed is
// retried on the next sync.
func (c *Config) Sync(ctx context.Context, force bool) (State, error) {
	unlock, err := c.lock()
	if err != nil {
		return State{}, err
	}
	defer unlock()
	prev, err := ReadState(c.WorkDir)
	if err != nil {
		return prev, err
	}
	repos, err := c.repos(ctx)
	if err != nil {
		return prev, err
	}

	next := State{Stacks: map[string]string{}, Apps: prev.Apps}
	var errs []error
	present := map[string]bool{}
	checkouts := map[string]string{} // every fetched repo, stack or not, for dashboards
	appsDone := false
	for _, r := range repos {
		commit, err := c.fetch(ctx, r)
		if err == nil {
			checkouts[r.Name] = filepath.Join(c.WorkDir, "repos", r.Name)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("fetch %s/%s: %w", c.Org, r.Name, err))
			if old, ok := prev.Stacks[r.Name]; ok {
				next.Stacks[r.Name] = old
				present[r.Name] = true
			}
			continue
		}
		dir := filepath.Join(c.WorkDir, "repos", r.Name)
		if _, err := os.Stat(filepath.Join(dir, "compose.yaml")); err != nil {
			continue // not a stack
		}
		present[r.Name] = true
		if !force && prev.Stacks[r.Name] == commit {
			next.Stacks[r.Name] = commit
			continue
		}
		systemFailed := len(errs) > 0
		if r.Name == LastStack && c.Apps {
			// Apps first: deploying the controller may end this process. A
			// broken app is reported but does not hold back the controller.
			apps, appErrs := c.syncApps(ctx, force, prev.Apps)
			next.Apps = apps
			errs = append(errs, appErrs...)
			appsDone = true
		}
		if r.Name == LastStack && systemFailed {
			// Replacing the controller now would restart it with work undone.
			next.Stacks[r.Name] = prev.Stacks[r.Name]
			continue
		}
		// Recorded before deploying the controller, which may end this process.
		if r.Name == LastStack {
			pending := State{Stacks: map[string]string{}, Apps: next.Apps, Applied: time.Now().UTC()}
			for k, v := range next.Stacks {
				pending.Stacks[k] = v
			}
			pending.Stacks[r.Name] = commit
			if err := c.writeState(pending); err != nil {
				return prev, err
			}
		}
		if err := system.Deploy(ctx, c.Run, system.Stack{Name: r.Name, Dir: dir}, c.Env); err != nil {
			errs = append(errs, fmt.Errorf("deploy %s: %w", r.Name, err))
			if old, ok := prev.Stacks[r.Name]; ok {
				next.Stacks[r.Name] = old // still running, at the old commit
			}
			continue
		}
		next.Stacks[r.Name] = commit
		log.Printf("deployed %s at %.12s", r.Name, commit)
	}
	for _, name := range prev.Names() {
		if present[name] {
			continue
		}
		if err := system.Remove(ctx, c.Run, name); err != nil {
			errs = append(errs, fmt.Errorf("remove %s: %w", name, err))
			next.Stacks[name] = prev.Stacks[name]
			continue
		}
		log.Printf("removed %s: its repo is gone or holds no compose.yaml", name)
	}
	if c.Apps && !appsDone {
		apps, appErrs := c.syncApps(ctx, force, prev.Apps)
		next.Apps = apps
		errs = append(errs, appErrs...)
	}
	next.Applied = time.Now().UTC()
	if err := c.writeState(next); err != nil {
		errs = append(errs, err)
	}
	// Dashboards follow every sync; Grafana may still be starting, so a
	// failure is logged and retried on the next one without holding up deploys.
	if c.Dashboards != nil {
		if err := c.Dashboards.Sync(ctx, c.Org, checkouts); err != nil {
			log.Printf("dashboards: %v", err)
		}
	}
	return next, errors.Join(errs...)
}

// Serve applies the namespace now and then follows it, checking every interval.
func (c *Config) Serve(ctx context.Context, interval time.Duration) error {
	if err := c.prepare(); err != nil {
		return err
	}
	force := true // deploy everything once on start, whatever the state says
	for {
		if _, err := c.Sync(ctx, force); err != nil {
			log.Printf("sync: %v", err)
		} else {
			force = false
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(interval):
		}
	}
}

// Once deploys the namespace a single time and returns the result.
func (c *Config) Once(ctx context.Context) (State, error) {
	if err := c.prepare(); err != nil {
		return State{}, err
	}
	return c.Sync(ctx, true)
}
