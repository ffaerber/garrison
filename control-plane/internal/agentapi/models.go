package agentapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"garrison/control-plane/internal/agents"
	"garrison/control-plane/internal/events"
	"garrison/control-plane/internal/gateway"
	"garrison/control-plane/internal/hooks"
	"garrison/control-plane/internal/models"
)

// Models sends each model to its provider, as llm/providers.yaml and
// llm/models.yaml of garrison's config say, and keeps models.yaml current:
// the model sync. Until those files exist, the install's one LLM serves.
type Models struct {
	Gitea *gateway.Gitea
	// KeyDir holds a key file per provider, named after it.
	KeyDir string
	// The install's LLM (install.env): written as the first provider when
	// garrison's config has none, and used until then. Its key is KeyFile
	// when KeyDir has none for it.
	InstallURL, InstallModel, InstallKey string
	HTTP                                 *http.Client
	Events                               events.Recorder
	Domain                               string

	mu     sync.Mutex
	l      gateway.LLM
	ok     bool
	read   time.Time
	warned map[string]string // agent and model: the day it was told
}

// cacheFor is how long the model files are kept before reading them again.
const cacheFor = time.Minute

func (m *Models) current(ctx context.Context) (gateway.LLM, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.read.IsZero() && time.Since(m.read) < cacheFor {
		return m.l, m.ok
	}
	l, ok, err := m.Gitea.LLM(ctx, "main")
	if err != nil {
		log.Printf("models: %v", err)
		m.read = time.Now().Add(-cacheFor + 5*time.Second) // try again soon, with what was read before
		return m.l, m.ok
	}
	if ok && l.Providers.Default == "" {
		ok = false // providers.yaml unreadable: as if there were none
	}
	m.l, m.ok, m.read = l, ok, time.Now()
	return l, ok
}

// install is the install's LLM.
func (m *Models) install() *LLM {
	return &LLM{URL: m.InstallURL, KeyFile: m.InstallKey, HTTP: m.HTTP}
}

// provider is the LLM of a provider: its key from KeyDir, or the install's
// key if it is the install's URL.
func (m *Models) provider(name string, p models.Provider) *LLM {
	key := filepath.Join(m.KeyDir, name)
	if _, err := os.Stat(key); err != nil && strings.TrimSuffix(p.URL, "/") == strings.TrimSuffix(m.InstallURL, "/") {
		key = m.InstallKey
	}
	return &LLM{URL: p.URL, KeyFile: key, HTTP: m.HTTP}
}

// Route is the provider and the model's name there for what agent names as
// its model, and its full id, <provider>/<model>. A model agents may not use
// (retired, turned off, unknown) is replaced by the default, and the feed
// told once a day.
func (m *Models) Route(ctx context.Context, agent, model string) (*LLM, string, string) {
	l, ok := m.current(ctx)
	if !ok {
		if model == "" {
			model = m.InstallModel
		}
		return m.install(), model, model
	}
	prov, name, full := l.Providers.Resolve(model)
	if why := models.Why(l.Providers, l.Catalog, model); why != "" && len(l.Catalog) > 0 {
		m.warn(ctx, agent, full, why)
		prov, name, full = l.Providers.Resolve("")
	}
	return m.provider(prov, l.Providers.Providers[prov]), name, full
}

func (m *Models) warn(ctx context.Context, agent, full, why string) {
	today := time.Now().Format(time.DateOnly)
	m.mu.Lock()
	if m.warned == nil {
		m.warned = map[string]string{}
	}
	told := m.warned[agent+" "+full] == today
	m.warned[agent+" "+full] = today
	m.mu.Unlock()
	if told {
		return
	}
	log.Printf("models: %s: %s; it uses the default", agent, why)
	if m.Events == nil || agent == "" {
		return
	}
	ns, name, _ := strings.Cut(agent, "/")
	m.Events.Record(ctx, events.Event{Source: events.SourceAgents, Kind: events.KindModel, Status: events.StatusRefused,
		Namespace: ns, Agent: agent, StartedBy: "agent " + agent,
		Title:  fmt.Sprintf("%s uses the default model: its own cannot be used", agent),
		Detail: why + fmt.Sprintf(". A root admin chooses another model in agents/%s/agent.yaml of garrison/config.", name),
		Link:   fmt.Sprintf("https://git.%s/%s/config/src/branch/main/agents/%s/agent.yaml", m.Domain, agents.PlatformNamespace, name)})
}

// list asks a provider for its models: GET <url>/models.
func (m *Models) list(ctx context.Context, name string, p models.Provider) ([]string, error) {
	l := m.provider(name, p)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(p.URL, "/")+"/models", nil)
	if err != nil {
		return nil, err
	}
	if key := l.key(); key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	client := &http.Client{Timeout: 30 * time.Second}
	if m.HTTP != nil {
		client = m.HTTP
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 500))
		return nil, fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(data)))
	}
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 32<<20)).Decode(&out); err != nil {
		return nil, err
	}
	var ids []string
	for _, d := range out.Data {
		if d.ID != "" && !slices.Contains(ids, d.ID) {
			ids = append(ids, d.ID)
		}
	}
	return ids, nil
}

// Sync asks every provider for its models and proposes the changed
// llm/models.yaml as a pull request; it returns that pull request's link,
// "" if nothing changed. Without providers.yaml, it writes the install's
// LLM as the first provider, and the models it offers, to main.
func (m *Models) Sync(ctx context.Context) (string, error) {
	g := m.Gitea
	rawP, err := g.File(ctx, models.Namespace, models.Repo, models.ProvidersFile)
	if err != nil {
		return "", err
	}
	seed := rawP == ""
	if seed {
		if m.InstallURL == "" {
			return "", errors.New("no providers.yaml, and no LLM in install.env to write one from")
		}
		rawP = string(models.Seed(m.InstallURL, m.InstallModel))
	}
	p, why := models.ParseProviders(rawP)
	if len(why) > 0 {
		return "", fmt.Errorf("%s", strings.Join(why, "; "))
	}
	rawM, err := g.File(ctx, models.Namespace, models.Repo, models.ModelsFile)
	if err != nil {
		return "", err
	}
	have, why := models.ParseCatalog(rawM)
	if len(why) > 0 {
		return "", fmt.Errorf("%s", strings.Join(why, "; "))
	}
	lists, errs := map[string][]string{}, map[string]string{}
	for name, pr := range p.Providers {
		ids, err := m.list(ctx, name, pr)
		if err != nil {
			errs[name] = err.Error()
			log.Printf("models: %s: %v", name, err)
			continue
		}
		lists[name] = ids
	}
	today := time.Now().Format(time.DateOnly)
	next, changes := models.Sync(p, have, lists, errs, today)
	want := next.Marshal()
	if seed {
		err := g.Commit(ctx, models.Namespace, models.Repo, "The model list, from the install's LLM", g.User,
			map[string][]byte{models.ProvidersFile: []byte(rawP), models.ModelsFile: want})
		m.invalidate()
		return "", err
	}
	if string(want) == rawM {
		// Nothing to propose: a pull request still open is out of date.
		_, err := g.ProposeFiles(ctx, models.Namespace, models.Repo, models.Branch, "", "", nil)
		return "", err
	}
	title, body, err := m.describe(ctx, changes, next)
	if err != nil {
		return "", err
	}
	return g.ProposeFiles(ctx, models.Namespace, models.Repo, models.Branch, title, body, map[string][]byte{models.ModelsFile: want})
}

func (m *Models) invalidate() {
	m.mu.Lock()
	m.read = time.Time{}
	m.mu.Unlock()
}

// describe is the pull request's title and description: what changed, and
// which agents name a model that is gone.
func (m *Models) describe(ctx context.Context, changes []models.Change, next models.Catalog) (string, string, error) {
	var parts []string
	var b strings.Builder
	b.WriteString("The providers' models changed. New models come turned off: set `enabled: true` for those agents may use. Merge to take the list.\n")
	for _, c := range changes {
		var what []string
		if n := len(c.Added); n > 0 {
			what = append(what, fmt.Sprintf("%d new", n))
		}
		if n := len(c.Gone); n > 0 {
			what = append(what, fmt.Sprintf("%d retired", n))
		}
		if n := len(c.Back); n > 0 {
			what = append(what, fmt.Sprintf("%d back", n))
		}
		if n := len(c.Dropped); n > 0 {
			what = append(what, fmt.Sprintf("%d dropped", n))
		}
		if len(what) == 0 {
			if c.Unreachable != "" {
				fmt.Fprintf(&b, "\n**%s** could not be asked, its models are left as they are: %s\n", c.Provider, c.Unreachable)
			}
			continue
		}
		parts = append(parts, c.Provider+": "+strings.Join(what, ", "))
		fmt.Fprintf(&b, "\n**%s**\n", c.Provider)
		for _, l := range []struct {
			name string
			ids  []string
		}{{"New", c.Added}, {"Retired", c.Gone}, {"Back", c.Back}, {"Dropped", c.Dropped}} {
			if len(l.ids) > 0 {
				fmt.Fprintf(&b, "- %s: %s\n", l.name, strings.Join(l.ids, ", "))
			}
		}
	}
	// Agents naming a model that is gone: they use the default until changed.
	l, _ := m.current(ctx)
	var stuck []string
	list, err := m.Gitea.Definitions(ctx)
	if err != nil {
		return "", "", err
	}
	for _, a := range list {
		if a.Model == "" {
			continue
		}
		_, _, full := l.Providers.Resolve(a.Model)
		if e, ok := next[full]; ok && e.Gone != "" {
			stuck = append(stuck, fmt.Sprintf("%s (%s)", a.Name, full))
		}
	}
	if len(stuck) > 0 {
		fmt.Fprintf(&b, "\n**Agents naming a retired model**, which use the default until their agent.yaml names another: %s\n", strings.Join(stuck, ", "))
	}
	b.WriteString("\nWritten by the model sync of the agent service.")
	return "Models: " + strings.Join(parts, "; "), b.String(), nil
}

// Run syncs at start, daily, and soon after providers.yaml changes.
func (m *Models) Run(ctx context.Context, check, every time.Duration) {
	var last string
	var at time.Time
	for {
		raw, err := m.Gitea.File(ctx, models.Namespace, models.Repo, models.ProvidersFile)
		switch {
		case err != nil:
			log.Printf("models: %v", err)
		case raw != last || time.Since(at) >= every:
			link, err := m.Sync(ctx)
			if err != nil {
				log.Printf("models: sync: %v", err)
				break
			}
			if raw == "" {
				// Seeded: read what was written next round.
				raw, _ = m.Gitea.File(ctx, models.Namespace, models.Repo, models.ProvidersFile)
			}
			last, at = raw, time.Now()
			if link != "" {
				log.Printf("models: proposed %s", link)
			}
		}
		if !hooks.Sleep(ctx, check, nil) {
			return
		}
	}
}
