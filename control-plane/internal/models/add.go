package models

import (
	"bytes"
	"errors"
	"fmt"
	"net/url"
	"path"
	"strings"

	"gopkg.in/yaml.v3"
)

// KeySecret is the secret of garrison that holds a provider's API key, set
// on the dashboard's Models page: secrets/llm-<provider>.yaml of its config,
// which the deployer writes to the agent service's key folder.
func KeySecret(provider string) string { return "llm-" + provider }

// ProviderOf is the provider whose key a secret of garrison holds; ok is
// false for any other secret.
func ProviderOf(secret string) (string, bool) {
	p, ok := strings.CutPrefix(secret, "llm-")
	return p, ok && ValidNewProvider(p)
}

// MaxNewProvider is how long a new provider's name may be: its key's secret,
// llm-<name>, is a secret name, at most 32.
const MaxNewProvider = 28

// ErrInvalidProvider: a name, URL or pattern that cannot be a new provider.
var ErrInvalidProvider = errors.New("invalid provider")

// ValidNewProvider tells whether name can be a provider added now.
func ValidNewProvider(name string) bool {
	return providerName.MatchString(name) && len(name) <= MaxNewProvider
}

// CheckProvider tells why name, rawURL and include cannot be a new
// provider, if they cannot. The URL is an OpenAI-compatible API's base, the
// one /chat/completions and /models are under, e.g. https://openrouter.ai/api/v1.
func CheckProvider(name, rawURL string, include []string) error {
	if !ValidNewProvider(name) {
		return fmt.Errorf("%w: a provider's name is lowercase letters, digits and dashes, at most %d, e.g. openrouter", ErrInvalidProvider, MaxNewProvider)
	}
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("%w: the URL is the API's base, http(s)://host/path, e.g. https://openrouter.ai/api/v1, without a key in it", ErrInvalidProvider)
	}
	if strings.HasSuffix(strings.TrimSuffix(u.Path, "/"), "/chat/completions") {
		return fmt.Errorf("%w: give the API's base, without /chat/completions: %s", ErrInvalidProvider, strings.TrimSuffix(strings.TrimSuffix(rawURL, "/"), "/chat/completions"))
	}
	for _, pat := range include {
		if _, err := path.Match(pat, ""); err != nil || pat == "" {
			return fmt.Errorf("%w: include %q is not a pattern such as anthropic/*", ErrInvalidProvider, pat)
		}
	}
	return nil
}

// AddProvider is providers.yaml with the provider added at the end of its
// providers, its comments and order kept. It refuses a provider that is
// there already, and a file it cannot read.
func AddProvider(raw, name, rawURL string, include []string) ([]byte, error) {
	if err := CheckProvider(name, rawURL, include); err != nil {
		return nil, err
	}
	have, why := ParseProviders(raw)
	if len(why) > 0 {
		return nil, fmt.Errorf("%s cannot be read, so nothing was changed: %s", ProvidersFile, strings.Join(why, "; "))
	}
	if _, ok := have.Providers[name]; ok {
		return nil, fmt.Errorf("%w: %s has a provider %s already", ErrInvalidProvider, ProvidersFile, name)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(raw), &doc); err != nil || len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("%s is not a mapping", ProvidersFile)
	}
	var providers *yaml.Node
	top := doc.Content[0]
	for i := 0; i+1 < len(top.Content); i += 2 {
		if top.Content[i].Value == "providers" {
			providers = top.Content[i+1]
		}
	}
	if providers == nil || providers.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("%s has no providers to add to", ProvidersFile)
	}
	// A flow mapping ({a: {url: ...}}) is written as a block from here on.
	providers.Style = 0
	str := func(s string) *yaml.Node { return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: s} }
	entry := &yaml.Node{Kind: yaml.MappingNode, Content: []*yaml.Node{str("url"), str(rawURL)}}
	if len(include) > 0 {
		seq := &yaml.Node{Kind: yaml.SequenceNode, Style: yaml.FlowStyle}
		for _, pat := range include {
			n := str(pat)
			n.Style = yaml.DoubleQuotedStyle
			seq.Content = append(seq.Content, n)
		}
		entry.Content = append(entry.Content, str("include"), seq)
	}
	providers.Content = append(providers.Content, str(name), entry)
	var b bytes.Buffer
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return nil, err
	}
	if _, why := ParseProviders(b.String()); len(why) > 0 {
		return nil, fmt.Errorf("the new %s would be refused: %s", ProvidersFile, strings.Join(why, "; "))
	}
	return b.Bytes(), nil
}
