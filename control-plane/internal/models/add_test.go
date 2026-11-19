package models

import (
	"errors"
	"strings"
	"testing"
)

const providersYAML = `# Garrison's LLM providers.
default: ollama-cloud/gpt-oss:120b
providers:
  # the install's
  ollama-cloud:
    url: https://ollama.com/v1
`

func TestAddProvider(t *testing.T) {
	out, err := AddProvider(providersYAML, "openrouter", "https://openrouter.ai/api/v1", []string{"anthropic/*"})
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, want := range []string{"# Garrison's LLM providers.", "# the install's", "openrouter:\n    url: https://openrouter.ai/api/v1\n    include: [\"anthropic/*\"]"} {
		if !strings.Contains(s, want) {
			t.Errorf("no %q in\n%s", want, s)
		}
	}
	if strings.Index(s, "ollama-cloud:") > strings.Index(s, "openrouter:") {
		t.Errorf("the new provider is not last:\n%s", s)
	}
	p, why := ParseProviders(s)
	if len(why) > 0 || p.Providers["openrouter"].URL != "https://openrouter.ai/api/v1" || p.Default != "ollama-cloud/gpt-oss:120b" {
		t.Errorf("%v %v", p, why)
	}
}

func TestAddProviderFlow(t *testing.T) {
	out, err := AddProvider("default: local/qwen\nproviders: {local: {url: 'http://10.0.0.2:11434/v1'}}\n", "groq", "https://api.groq.com/openai/v1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if p, why := ParseProviders(string(out)); len(why) > 0 || len(p.Providers) != 2 {
		t.Errorf("%s: %v", out, why)
	}
}

func TestAddProviderRefuses(t *testing.T) {
	for _, c := range []struct{ name, url string }{
		{"ollama-cloud", "https://ollama.com/v1"},                 // there already
		{"Open_Router", "https://openrouter.ai/api/v1"},           // not a name
		{strings.Repeat("a", 29), "https://openrouter.ai/api/v1"}, // too long for its secret
		{"x", "ftp://example.com"},
		{"x", "https://user:key@example.com/v1"},
		{"x", "https://example.com/v1?key=1"},
		{"x", "https://example.com/v1/chat/completions"},
	} {
		if _, err := AddProvider(providersYAML, c.name, c.url, nil); !errors.Is(err, ErrInvalidProvider) {
			t.Errorf("%s %s: %v", c.name, c.url, err)
		}
	}
	if _, err := AddProvider(providersYAML, "x", "https://example.com/v1", []string{"[a"}); !errors.Is(err, ErrInvalidProvider) {
		t.Errorf("a bad pattern: %v", err)
	}
	if _, err := AddProvider("default: nope\n", "x", "https://example.com/v1", nil); err == nil {
		t.Error("a file that cannot be read")
	}
}

func TestKeySecret(t *testing.T) {
	if p, ok := ProviderOf(KeySecret("openrouter")); !ok || p != "openrouter" {
		t.Error(p, ok)
	}
	for _, s := range []string{"api-token", "llm-", "llm-Bad"} {
		if _, ok := ProviderOf(s); ok {
			t.Error(s)
		}
	}
}
