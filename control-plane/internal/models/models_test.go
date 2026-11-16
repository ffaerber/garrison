package models

import (
	"slices"
	"strings"
	"testing"
)

const providers = `default: ollama-cloud/gpt-oss:120b
providers:
  ollama-cloud:
    url: https://ollama.com/v1
  openrouter:
    url: https://openrouter.ai/api/v1
    include: ["anthropic/*"]
`

func TestProviders(t *testing.T) {
	p, why := ParseProviders(providers)
	if len(why) > 0 {
		t.Fatal(why)
	}
	for _, c := range []struct{ in, prov, model, full string }{
		{"", "ollama-cloud", "gpt-oss:120b", "ollama-cloud/gpt-oss:120b"},
		{"qwen3-coder", "ollama-cloud", "qwen3-coder", "ollama-cloud/qwen3-coder"},
		{"openrouter/anthropic/claude-sonnet-5.5", "openrouter", "anthropic/claude-sonnet-5.5", "openrouter/anthropic/claude-sonnet-5.5"},
		// Not a provider: a model of the default one, as it is.
		{"anthropic/claude", "ollama-cloud", "anthropic/claude", "ollama-cloud/anthropic/claude"},
	} {
		prov, model, full := p.Resolve(c.in)
		if prov != c.prov || model != c.model || full != c.full {
			t.Errorf("%q: %s %s %s", c.in, prov, model, full)
		}
	}
	for _, bad := range []string{
		"providers: {}\ndefault: x/y\n",
		"default: nope/m\nproviders:\n  a:\n    url: https://a.example\n",
		"default: a/m\nproviders:\n  A_b:\n    url: https://a.example\n",
		"default: a/m\nproviders:\n  a:\n    url: ftp://a.example\n",
		"default: a/m\nproviders:\n  a:\n    url: https://a.example\n    include: ['[']\n",
		"default: a/m\nproviders:\n  a:\n    url: https://a.example\n    key: hunter2\n",
	} {
		if _, why := ParseProviders(bad); len(why) == 0 {
			t.Errorf("taken: %s", bad)
		}
	}
}

func TestTheSync(t *testing.T) {
	p, _ := ParseProviders(providers)
	lists := map[string][]string{
		"ollama-cloud": {"gpt-oss:120b", "qwen3-coder"},
		"openrouter":   {"anthropic/claude-sonnet-5.5", "openai/gpt-6", "anthropic/claude-haiku-5.5"},
	}
	// The first sync: the default comes enabled, the rest off; openrouter's
	// only as far as include lets them.
	c, changes := Sync(p, Catalog{}, lists, nil, "2026-11-16")
	if got := c.Enabled(); !slices.Equal(got, []string{"ollama-cloud/gpt-oss:120b"}) {
		t.Errorf("enabled: %v", got)
	}
	if _, ok := c["openrouter/openai/gpt-6"]; ok || len(c) != 4 {
		t.Errorf("catalog: %v", c)
	}
	if len(changes) != 2 || len(changes[1].Added) != 2 {
		t.Errorf("changes: %+v", changes)
	}
	if why := Check(p, c); len(why) > 0 {
		t.Error(why)
	}

	// Marshalled and read back, the same.
	back, why := ParseCatalog(string(c.Marshal()))
	if len(why) > 0 || len(back) != len(c) || back["ollama-cloud/qwen3-coder"] != c["ollama-cloud/qwen3-coder"] {
		t.Fatalf("round trip: %v %v\n%s", back, why, c.Marshal())
	}

	// A person enables one; it retires, another comes; openrouter is down.
	m := c["ollama-cloud/qwen3-coder"]
	m.Enabled = true
	c["ollama-cloud/qwen3-coder"] = m
	lists = map[string][]string{"ollama-cloud": {"gpt-oss:120b", "glm-5"}}
	c2, changes := Sync(p, c, lists, map[string]string{"openrouter": "502"}, "2026-12-01")
	if c2["ollama-cloud/qwen3-coder"].Gone != "2026-12-01" || !c2["ollama-cloud/qwen3-coder"].Enabled {
		t.Errorf("not gone: %+v", c2["ollama-cloud/qwen3-coder"])
	}
	if c2["ollama-cloud/glm-5"].Enabled || c2["ollama-cloud/glm-5"].Added != "2026-12-01" {
		t.Errorf("new: %+v", c2["ollama-cloud/glm-5"])
	}
	if c2["openrouter/anthropic/claude-sonnet-5.5"] != c["openrouter/anthropic/claude-sonnet-5.5"] {
		t.Error("an unreachable provider's models changed")
	}
	if changes[1].Unreachable != "502" || !changes[1].Empty() {
		t.Errorf("openrouter: %+v", changes[1])
	}
	if why := Why(p, c2, "qwen3-coder"); !strings.Contains(why, "retired") || !strings.Contains(why, "gpt-oss:120b") {
		t.Errorf("why: %s", why)
	}
	if why := Why(p, c2, "ollama-cloud/glm-5"); !strings.Contains(why, "not enabled") {
		t.Errorf("why: %s", why)
	}
	if why := Why(p, c2, "gpt-oss:120b"); why != "" {
		t.Errorf("why: %s", why)
	}

	// It comes back; nothing changes a second time.
	lists["ollama-cloud"] = append(lists["ollama-cloud"], "qwen3-coder")
	c3, changes := Sync(p, c2, lists, nil, "2026-12-02")
	if c3["ollama-cloud/qwen3-coder"].Gone != "" || len(changes[0].Back) != 1 {
		t.Errorf("not back: %+v", changes)
	}
	if _, changes := Sync(p, c3, lists, nil, "2026-12-03"); !changes[0].Empty() {
		t.Errorf("changed again: %+v", changes)
	}

	// A provider removed: its models are dropped.
	p2, _ := ParseProviders("default: ollama-cloud/gpt-oss:120b\nproviders:\n  ollama-cloud:\n    url: https://ollama.com/v1\n")
	c4, changes := Sync(p2, c3, lists, nil, "2026-12-04")
	if len(c4) != 3 || len(changes) != 2 || len(changes[1].Dropped) != 2 {
		t.Errorf("dropped: %v %+v", c4, changes)
	}

	// The default no longer offered: the check refuses it.
	c5, _ := Sync(p, c3, map[string][]string{"ollama-cloud": {"glm-5"}}, nil, "2026-12-05")
	if why := Check(p, c5); len(why) != 1 || !strings.Contains(why[0], "gone") {
		t.Errorf("check: %v", why)
	}
}

func TestProviderName(t *testing.T) {
	for in, want := range map[string]string{
		"https://ollama.com/v1":        "ollama-cloud",
		"https://openrouter.ai/api/v1": "openrouter",
		"https://api.openai.com/v1":    "openai",
		"http://10.97.0.1:11434/v1":    "local",
		"http://ollama:11434/v1":       "local",
		"https://api.together.xyz/v1":  "together",
		"https://llm.example.com/v1":   "llm",
		"https://203.0.113.9/v1":       "llm",
	} {
		if got := ProviderName(in); got != want {
			t.Errorf("%s: %s, want %s", in, got, want)
		}
	}
	p, why := ParseProviders(string(Seed("http://10.97.0.1:11434/v1", "qwen3.8-64k:latest")))
	if len(why) > 0 || p.Default != "local/qwen3.8-64k:latest" {
		t.Errorf("seed: %+v %v", p, why)
	}
}
