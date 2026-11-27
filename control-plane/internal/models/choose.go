package models

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// The Models page chooses the default model and the agents that use
// another in one change: providers.yaml's default, the chosen models
// enabled in models.yaml, and the model line of each agent.yaml. The files
// are changed line by line, so their comments and layout stay as they are.

// Offered are the models one may choose: every model of the catalog its
// provider still offers, enabled or not, sorted.
func (c Catalog) Offered() []string {
	var out []string
	for _, id := range sortedKeys(c) {
		if c[id].Gone == "" {
			out = append(out, id)
		}
	}
	return out
}

// Enable is the catalog with ids enabled, and whether that changed it.
func (c Catalog) Enable(ids ...string) (Catalog, bool) {
	out, changed := Catalog{}, false
	for id, m := range c {
		out[id] = m
	}
	for _, id := range ids {
		m, ok := out[id]
		if !ok || m.Enabled {
			continue
		}
		m.Enabled = true
		out[id] = m
		changed = true
	}
	return out, changed
}

// topLine matches a top-level key's line: the key, its value, and a
// comment after it, if any.
func topLine(key string) *regexp.Regexp {
	return regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(key) + `:[ \t]*("(?:[^"\\]|\\.)*"|'[^']*'|[^#\n]*?)([ \t]+#[^\n]*)?[ \t]*$`)
}

// setTop is raw with the top-level key set to the string value, the line's
// comment kept; a key not there is added at the end. It checks that the
// file reads back with that value.
func setTop(file, raw, key, value string) ([]byte, error) {
	q, _ := json.Marshal(value) // a JSON string is a YAML one
	re := topLine(key)
	var out string
	switch n := len(re.FindAllStringIndex(raw, -1)); {
	case n > 1:
		return nil, fmt.Errorf("%s has %s twice", file, key)
	case n == 1:
		out = re.ReplaceAllStringFunc(raw, func(line string) string {
			m := re.FindStringSubmatch(line)
			return key + ": " + string(q) + m[2]
		})
	default:
		out = raw
		if out != "" && !strings.HasSuffix(out, "\n") {
			out += "\n"
		}
		out += key + ": " + string(q) + "\n"
	}
	var back map[string]any
	if err := yaml.Unmarshal([]byte(out), &back); err != nil {
		return nil, fmt.Errorf("%s cannot be read, so nothing was changed: %v", file, err)
	}
	if got, _ := back[key].(string); got != value {
		return nil, fmt.Errorf("%s: its %s could not be changed line by line; change it by hand", file, key)
	}
	return []byte(out), nil
}

// SetDefault is providers.yaml with id, <provider>/<model> of a provider it
// lists, as the default model.
func SetDefault(raw, id string) ([]byte, error) {
	p, why := ParseProviders(raw)
	if len(why) > 0 {
		return nil, fmt.Errorf("%s cannot be read, so nothing was changed: %s", ProvidersFile, strings.Join(why, "; "))
	}
	if prov, model := p.split(id); prov == "" || model == "" {
		return nil, fmt.Errorf("%w: the default %q must be <provider>/<model> of a provider of %s", ErrInvalidProvider, id, ProvidersFile)
	}
	out, err := setTop(ProvidersFile, raw, "default", id)
	if err != nil {
		return nil, err
	}
	if _, why := ParseProviders(string(out)); len(why) > 0 {
		return nil, fmt.Errorf("the new %s would be refused: %s", ProvidersFile, strings.Join(why, "; "))
	}
	return out, nil
}

// SetAgentModel is an agent.yaml with model set: <provider>/<model>, or ""
// for the default.
func SetAgentModel(file, raw, model string) ([]byte, error) {
	return setTop(file, raw, "model", model)
}
