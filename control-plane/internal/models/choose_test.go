package models

import (
	"strings"
	"testing"
)

func TestSetDefaultKeepsTheFile(t *testing.T) {
	raw := "# The providers.\ndefault: \"local/qwen3.8-27b\"   # the install's\nproviders:\n  local:\n    url: \"http://10.0.0.2:11434/v1\"\n  ollama-cloud:\n    url: https://ollama.com/v1\n"
	out, err := SetDefault(raw, "ollama-cloud/glm-5.3")
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Replace(raw, `"local/qwen3.8-27b"`, `"ollama-cloud/glm-5.3"`, 1)
	if string(out) != want {
		t.Errorf("got\n%s\nwant\n%s", out, want)
	}
	for _, bad := range []string{"nowhere/glm", "glm-5.3", "ollama-cloud/"} {
		if _, err := SetDefault(raw, bad); err == nil {
			t.Errorf("%q taken as the default", bad)
		}
	}
}

func TestSetAgentModel(t *testing.T) {
	for _, c := range []struct{ raw, model, want string }{
		{"description: Codes\nmodel: \"\"\ntools: [read_file]\n", "ollama-cloud/glm-5.3", "description: Codes\nmodel: \"ollama-cloud/glm-5.3\"\ntools: [read_file]\n"},
		{"description: Ops\nmodel: \"\"     # the default; or an enabled model\ntools: all\n", "local/qwen", "description: Ops\nmodel: \"local/qwen\"     # the default; or an enabled model\ntools: all\n"},
		{"model: openrouter/anthropic/claude-sonnet-5.5\n", "", "model: \"\"\n"},
		{"description: No model line\ntools: all", "local/qwen", "description: No model line\ntools: all\nmodel: \"local/qwen\"\n"},
		{"model: 'local/a:b' # quoted\n", "local/c", "model: \"local/c\" # quoted\n"},
	} {
		out, err := SetAgentModel("agents/x/agent.yaml", c.raw, c.model)
		if err != nil {
			t.Errorf("%q: %v", c.raw, err)
			continue
		}
		if string(out) != c.want {
			t.Errorf("got\n%s\nwant\n%s", out, c.want)
		}
	}
	// A model line inside something else is not the agent's.
	if _, err := SetAgentModel("a", "model: a\nmodel: b\n", "c"); err == nil {
		t.Error("a file with model twice was changed")
	}
	out, err := SetAgentModel("a", "skills:\n  model: x\n", "local/q")
	if err != nil || !strings.HasSuffix(string(out), "  model: x\nmodel: \"local/q\"\n") {
		t.Errorf("%q %v", out, err)
	}
}

func TestEnableAndOffered(t *testing.T) {
	c := Catalog{"a/on": {Enabled: true}, "a/off": {}, "a/gone": {Gone: "2026-11-01"}}
	if got := strings.Join(c.Offered(), " "); got != "a/off a/on" {
		t.Errorf("offered: %s", got)
	}
	next, changed := c.Enable("a/on", "a/off", "a/unknown")
	if !changed || !next["a/off"].Enabled || c["a/off"].Enabled {
		t.Errorf("enable: %v %v (the original must stay)", changed, next)
	}
	if _, ok := next["a/unknown"]; ok {
		t.Error("enabled a model not listed")
	}
	if _, changed := c.Enable("a/on"); changed {
		t.Error("an enabled model changed the catalog")
	}
}
