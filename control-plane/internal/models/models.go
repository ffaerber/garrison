// Package models reads and writes the LLMs Garrison's agents use, kept
// in garrison's config repo: llm/providers.yaml (the providers and the default
// model, changed by root admins) and llm/models.yaml (every model the
// providers offer, written by the model sync, enabled by hand).
package models

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	// Unit and Repo hold the files.
	Unit          = "garrison"
	Repo          = "config"
	ProvidersFile = "llm/providers.yaml"
	ModelsFile    = "llm/models.yaml"
	// Branch starts the names of the model sync's branches: garrison/models-<time>.
	Branch = "garrison/models"
)

var (
	providerName = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)
	modelID      = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/@+-]{0,199}$`)
)

// Provider is an OpenAI-compatible API.
type Provider struct {
	URL string `yaml:"url"`
	// Include, if set, keeps only the models matching one of these patterns
	// (path.Match, e.g. "anthropic/*").
	Include []string `yaml:"include,omitempty"`
}

// Providers is llm/providers.yaml.
type Providers struct {
	Default   string              `yaml:"default"` // <provider>/<model>
	Providers map[string]Provider `yaml:"providers"`
}

// ParseProviders reads llm/providers.yaml; problems say what is wrong.
func ParseProviders(raw string) (Providers, []string) {
	var p Providers
	dec := yaml.NewDecoder(strings.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(&p); err != nil && !errors.Is(err, io.EOF) {
		return Providers{}, []string{ProvidersFile + ": " + err.Error()}
	}
	var why []string
	if len(p.Providers) == 0 {
		why = append(why, ProvidersFile+": no providers")
	}
	for _, name := range sortedKeys(p.Providers) {
		pr := p.Providers[name]
		if !providerName.MatchString(name) || len(name) > 40 {
			why = append(why, fmt.Sprintf("%s: provider %q: a name is lowercase letters, digits and dashes, at most 40", ProvidersFile, name))
		}
		if u, err := url.Parse(pr.URL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			why = append(why, fmt.Sprintf("%s: provider %s: url %q is not an http(s) URL", ProvidersFile, name, pr.URL))
		}
		for _, pat := range pr.Include {
			if _, err := path.Match(pat, ""); err != nil {
				why = append(why, fmt.Sprintf("%s: provider %s: include %q is not a pattern", ProvidersFile, name, pat))
			}
		}
	}
	if prov, model := p.split(p.Default); prov == "" || model == "" || !strings.HasPrefix(p.Default, prov+"/") {
		why = append(why, fmt.Sprintf("%s: default %q must be <provider>/<model> of a provider listed", ProvidersFile, p.Default))
	}
	return p, why
}

// split is Resolve without the default: provider "" if id names none.
func (p Providers) split(id string) (string, string) {
	if prov, model, ok := strings.Cut(id, "/"); ok {
		if _, known := p.Providers[prov]; known {
			return prov, model
		}
	}
	return "", id
}

// Resolve takes the model an agent names, "<provider>/<model>" or a model of
// the default provider, empty for the default, to its provider, the model's
// name at the provider, and its full id.
func (p Providers) Resolve(id string) (provider, model, full string) {
	if id == "" {
		id = p.Default
	}
	provider, model = p.split(id)
	if provider == "" {
		provider, _ = p.split(p.Default)
	}
	return provider, model, provider + "/" + model
}

// Model is one entry of llm/models.yaml.
type Model struct {
	Enabled bool   `yaml:"enabled" json:"enabled"`
	Added   string `yaml:"added,omitempty" json:"added,omitempty"` // the date the sync first saw it
	Gone    string `yaml:"gone,omitempty" json:"gone,omitempty"`   // the date its provider stopped offering it
}

// Usable tells whether agents may use the model.
func (m Model) Usable() bool { return m.Enabled && m.Gone == "" }

// Catalog is llm/models.yaml: models by full id, <provider>/<model>.
type Catalog map[string]Model

type catalogFile struct {
	Models Catalog `yaml:"models"`
}

// ParseCatalog reads llm/models.yaml.
func ParseCatalog(raw string) (Catalog, []string) {
	var f catalogFile
	dec := yaml.NewDecoder(strings.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil && !errors.Is(err, io.EOF) {
		return nil, []string{ModelsFile + ": " + err.Error()}
	}
	var why []string
	for _, id := range sortedKeys(f.Models) {
		if !modelID.MatchString(id) || !strings.Contains(id, "/") {
			why = append(why, fmt.Sprintf("%s: %q is not <provider>/<model>", ModelsFile, id))
		}
	}
	if f.Models == nil {
		f.Models = Catalog{}
	}
	return f.Models, why
}

// Check says what is wrong with the two files together: the default model
// must be enabled. Models of a provider no longer listed are left for the
// sync to drop.
func Check(p Providers, c Catalog) []string {
	_, _, def := p.Resolve("")
	m, ok := c[def]
	switch {
	case !ok:
		return []string{fmt.Sprintf("%s: the default model %s is not listed", ModelsFile, def)}
	case m.Gone != "":
		return []string{fmt.Sprintf("%s: the default model %s is gone since %s; choose another default in %s", ModelsFile, def, m.Gone, ProvidersFile)}
	case !m.Enabled:
		return []string{fmt.Sprintf("%s: the default model %s must be enabled", ModelsFile, def)}
	}
	return nil
}

// Enabled are the ids of the models agents may use.
func (c Catalog) Enabled() []string {
	var out []string
	for _, id := range sortedKeys(c) {
		if c[id].Usable() {
			out = append(out, id)
		}
	}
	return out
}

// Why tells why an agent may not name model, "" if it may.
func Why(p Providers, c Catalog, model string) string {
	if model == "" {
		return ""
	}
	_, _, full := p.Resolve(model)
	m, ok := c[full]
	var why string
	switch {
	case !ok:
		why = fmt.Sprintf("model %s is not a model of Garrison's providers", full)
	case m.Gone != "":
		why = fmt.Sprintf("model %s is retired: its provider stopped offering it on %s", full, m.Gone)
	case !m.Enabled:
		why = fmt.Sprintf("model %s is not enabled", full)
	default:
		return ""
	}
	return why + fmt.Sprintf("; empty is the default, %s; the enabled ones: %s (garrison/config %s)", p.Default, strings.Join(c.Enabled(), ", "), ModelsFile)
}

// Marshal writes llm/models.yaml: one line per model, sorted.
func (c Catalog) Marshal() []byte {
	var b strings.Builder
	b.WriteString("# Every model the providers of providers.yaml offer, written by the model sync.\n")
	b.WriteString("# Set enabled: true to let agents use one. gone: the date its provider stopped\n")
	b.WriteString("# offering it; agents that name it use the default model instead.\n")
	if len(c) == 0 {
		b.WriteString("models: {}\n")
		return []byte(b.String())
	}
	b.WriteString("models:\n")
	for _, id := range sortedKeys(c) {
		m := c[id]
		key, _ := json.Marshal(id) // a JSON string is a YAML one
		fmt.Fprintf(&b, "  %s: {enabled: %t", key, m.Enabled)
		if m.Added != "" {
			fmt.Fprintf(&b, ", added: %s", m.Added)
		}
		if m.Gone != "" {
			fmt.Fprintf(&b, ", gone: %s", m.Gone)
		}
		b.WriteString("}\n")
	}
	return []byte(b.String())
}

// Change is what a sync changed of one provider's models.
type Change struct {
	Provider          string
	Added, Gone, Back []string // full ids
	Dropped           []string // of a provider no longer listed, or no longer included
	Unreachable       string   // why its models could not be listed
}

// Empty tells whether nothing changed.
func (c Change) Empty() bool {
	return len(c.Added)+len(c.Gone)+len(c.Back)+len(c.Dropped) == 0
}

// Sync takes the models each provider lists now (lists; a provider missing
// from it could not be asked, errs says why) into the catalog: new ones are
// added, turned off but for the default; ones no longer listed are marked
// gone, and unmarked should they come back. Models of a provider that could
// not be asked stay as they are.
func Sync(p Providers, have Catalog, lists map[string][]string, errs map[string]string, today string) (Catalog, []Change) {
	out := Catalog{}
	_, _, def := p.Resolve("")
	byProvider := map[string]*Change{}
	change := func(prov string) *Change {
		if c, ok := byProvider[prov]; ok {
			return c
		}
		byProvider[prov] = &Change{Provider: prov}
		return byProvider[prov]
	}
	for _, prov := range sortedKeys(p.Providers) {
		change(prov)
	}
	// What is there: kept, gone, back or dropped.
	for _, id := range sortedKeys(have) {
		m := have[id]
		prov, name := p.split(id)
		if prov == "" {
			before, _, _ := strings.Cut(id, "/")
			change(before).Dropped = append(change(before).Dropped, id)
			continue
		}
		list, asked := lists[prov]
		if !asked {
			out[id] = m
			continue
		}
		offered := slices.Contains(list, name)
		if !m.Enabled && !included(p.Providers[prov], name) && id != def {
			change(prov).Dropped = append(change(prov).Dropped, id)
			continue
		}
		switch {
		case offered && m.Gone != "":
			m.Gone = ""
			change(prov).Back = append(change(prov).Back, id)
		case !offered && m.Gone == "":
			m.Gone = today
			change(prov).Gone = append(change(prov).Gone, id)
		}
		out[id] = m
	}
	// What is new.
	for _, prov := range sortedKeys(p.Providers) {
		list, asked := lists[prov]
		if !asked {
			change(prov).Unreachable = errs[prov]
			continue
		}
		for _, name := range list {
			id := prov + "/" + name
			if _, ok := out[id]; ok || !modelID.MatchString(id) || !included(p.Providers[prov], name) && id != def {
				continue
			}
			out[id] = Model{Enabled: id == def, Added: today}
			change(prov).Added = append(change(prov).Added, id)
		}
		slices.Sort(change(prov).Added)
	}
	// The default is always listed: gone if its provider was asked and did
	// not offer it, which the check then refuses.
	if _, ok := out[def]; !ok {
		prov, _ := p.split(def)
		m := Model{Enabled: true, Added: today}
		if _, asked := lists[prov]; asked {
			m.Gone = today
			change(prov).Gone = append(change(prov).Gone, def)
		} else {
			change(prov).Added = append(change(prov).Added, def)
		}
		out[def] = m
	}
	changes := make([]Change, 0, len(byProvider))
	for _, prov := range sortedKeys(byProvider) {
		changes = append(changes, *byProvider[prov])
	}
	return out, changes
}

func included(pr Provider, name string) bool {
	if len(pr.Include) == 0 {
		return true
	}
	for _, pat := range pr.Include {
		if ok, _ := path.Match(pat, name); ok {
			return true
		}
	}
	return false
}

// ProviderName names the provider of an LLM URL, for the providers.yaml
// written at install: ollama-cloud for ollama.com, openrouter, openai, and
// local for a private address.
func ProviderName(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" {
		return "llm"
	}
	host := u.Hostname()
	switch {
	case host == "ollama.com" || strings.HasSuffix(host, ".ollama.com"):
		return "ollama-cloud"
	case host == "openrouter.ai" || strings.HasSuffix(host, ".openrouter.ai"):
		return "openrouter"
	case host == "api.openai.com":
		return "openai"
	case host == "localhost" || !strings.Contains(host, "."):
		return "local"
	}
	if ip := net.ParseIP(host); ip != nil {
		if ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
			return "local"
		}
		return "llm"
	}
	labels := strings.Split(strings.TrimPrefix(host, "api."), ".")
	if name := strings.ToLower(labels[0]); providerName.MatchString(name) && len(name) <= 40 {
		return name
	}
	return "llm"
}

// Seed is the providers.yaml written at install, from its answers.
func Seed(baseURL, model string) []byte {
	name := ProviderName(baseURL)
	var b strings.Builder
	b.WriteString("# The LLM providers of Garrison's agents: OpenAI-compatible APIs. See README.md.\n")
	b.WriteString("# A provider's key is the file /etc/garrison/llm-keys/<provider> on the machine, never in Git.\n")
	def, _ := json.Marshal(name + "/" + model)
	u, _ := json.Marshal(baseURL)
	fmt.Fprintf(&b, "default: %s\nproviders:\n  %s:\n    url: %s\n", def, name, u)
	return []byte(b.String())
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
