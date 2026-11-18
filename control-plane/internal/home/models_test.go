package home

import (
	"context"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"testing"

	"garrison/control-plane/internal/changes"
	"garrison/control-plane/internal/events"
	"garrison/control-plane/internal/models"
	"garrison/control-plane/internal/secrets"
)

// rootKeys: alice, a root admin, keeps garrison's keys.
type rootKeys struct{ values map[string]string }

func (f *rootKeys) Namespaces(context.Context, string) ([]string, error) { return nil, nil }
func (f *rootKeys) May(_ context.Context, ns, p string) (bool, error) {
	return ns == "garrison" && p == "alice", nil
}
func (f *rootKeys) Names(_ context.Context, ns string) ([]string, error) {
	var out []string
	for k := range f.values {
		if n, ok := strings.CutPrefix(k, ns+"/"); ok {
			out = append(out, n)
		}
	}
	slices.Sort(out)
	return out, nil
}
func (f *rootKeys) Set(ctx context.Context, ns, name, value, p string) error {
	if ok, _ := f.May(ctx, ns, p); !ok {
		return secrets.ErrForbidden
	}
	f.values[ns+"/"+name] = value
	return nil
}
func (f *rootKeys) Delete(ctx context.Context, ns, name, p string) error {
	if ok, _ := f.May(ctx, ns, p); !ok {
		return secrets.ErrForbidden
	}
	if _, ok := f.values[ns+"/"+name]; !ok {
		return fmt.Errorf("%w: no %s", secrets.ErrInvalid, name)
	}
	delete(f.values, ns+"/"+name)
	return nil
}

type fakeProviders struct {
	p        models.Providers
	c        models.Catalog
	proposed []string
}

func (f *fakeProviders) Providers(context.Context) (models.Providers, models.Catalog, bool, error) {
	return f.p, f.c, true, nil
}
func (f *fakeProviders) ProposeProvider(_ context.Context, person, name, apiURL string, include []string) (int, error) {
	if person != "alice" {
		return 0, changes.ErrForbidden
	}
	if err := models.CheckProvider(name, apiURL, include); err != nil {
		return 0, err
	}
	f.proposed = append(f.proposed, fmt.Sprintf("%s %s %s %v", person, name, apiURL, include))
	return 9, nil
}

func TestTheModelsPage(t *testing.T) {
	s, m := server()
	keys := &rootKeys{values: map[string]string{"garrison/llm-ollama-cloud": "sk-ollama"}}
	prov := &fakeProviders{
		p: models.Providers{Default: "ollama-cloud/gpt-oss:120b", Providers: map[string]models.Provider{"ollama-cloud": {URL: "https://ollama.com/v1"}}},
		c: models.Catalog{"ollama-cloud/gpt-oss:120b": {Enabled: true}, "ollama-cloud/qwen": {}},
	}
	s.Secrets, s.Providers = keys, prov
	origin := "https://home.test"

	// alice, a root admin, sees the providers, never a key; the nav offers it.
	w := get(s, "alice", "/models")
	body := w.Body.String()
	if w.Code != 200 || !strings.Contains(body, "https://ollama.com/v1") || !strings.Contains(body, "1 of 2 models on") ||
		!strings.Contains(body, `href="/models"`) || strings.Contains(body, "sk-ollama") {
		t.Fatalf("page: %d\n%s", w.Code, body)
	}
	// dave is not a root admin: no page, and no link to it.
	if w := get(s, "dave", "/models"); w.Code != 403 {
		t.Errorf("dave: %d", w.Code)
	}
	if w := get(s, "dave", "/secrets"); strings.Contains(w.Body.String(), `href="/models"`) {
		t.Error("dave's nav links the Models page")
	}

	// Adding a provider stores its key, then proposes it, and goes to its card.
	form := url.Values{"action": {"add"}, "name": {"openrouter"}, "url": {"https://openrouter.ai/api/v1"}, "key": {"sk-or-1"}, "include": {"anthropic/* openai/gpt-6*"}}
	w = post(s, "alice", origin, "/models", form)
	if w.Code != 303 || w.Header().Get("Location") != "/c/garrison/config/9" {
		t.Fatalf("add: %d %s\n%s", w.Code, w.Header().Get("Location"), w.Body)
	}
	if keys.values["garrison/llm-openrouter"] != "sk-or-1" || len(prov.proposed) != 1 || prov.proposed[0] != "alice openrouter https://openrouter.ai/api/v1 [anthropic/* openai/gpt-6*]" {
		t.Errorf("%v %v", keys.values, prov.proposed)
	}
	es, _ := m.List(context.Background(), events.Query{Visibility: events.Visibility{All: true}, Kinds: []string{events.KindSecret}})
	if !slices.ContainsFunc(es, func(e events.Event) bool {
		return strings.Contains(e.Title, "alice set the API key of the LLM provider openrouter") && !strings.Contains(e.Title, "sk-or")
	}) {
		t.Error("the feed has no key set")
	}

	// A provider listed already: only its key is replaced, nothing proposed.
	w = post(s, "alice", origin, "/models", url.Values{"action": {"add"}, "name": {"ollama-cloud"}, "key": {"sk-ollama-2"}})
	if w.Code != 303 || !strings.HasPrefix(w.Header().Get("Location"), "/models?") || keys.values["garrison/llm-ollama-cloud"] != "sk-ollama-2" || len(prov.proposed) != 1 {
		t.Errorf("replace: %d %s %v", w.Code, w.Header().Get("Location"), prov.proposed)
	}
	if w := get(s, "alice", w.Header().Get("Location")); !strings.Contains(w.Body.String(), "Saved the key of ollama-cloud") {
		t.Errorf("no notice:\n%s", w.Body)
	}

	// Refused, and no key left behind: a bad name or URL, an empty key, a
	// foreign page, someone who is not a root admin.
	for _, c := range []struct {
		who, origin string
		form        url.Values
		code        int
	}{
		{"alice", origin, url.Values{"action": {"add"}, "name": {"Groq"}, "url": {"https://api.groq.com/openai/v1"}, "key": {"k"}}, 400},
		{"alice", origin, url.Values{"action": {"add"}, "name": {"groq"}, "url": {"https://api.groq.com/openai/v1/chat/completions"}, "key": {"k"}}, 400},
		{"alice", origin, url.Values{"action": {"add"}, "name": {"groq"}, "url": {"https://api.groq.com/openai/v1"}, "key": {""}}, 400},
		{"alice", "https://todo.home.test", url.Values{"action": {"add"}, "name": {"groq"}, "url": {"https://api.groq.com/openai/v1"}, "key": {"k"}}, 403},
		{"dave", origin, url.Values{"action": {"add"}, "name": {"groq"}, "url": {"https://api.groq.com/openai/v1"}, "key": {"k"}}, 403},
		{"alice", origin, url.Values{"action": {"key"}, "name": {"groq"}, "key": {"k"}}, 400},
	} {
		if w := post(s, c.who, c.origin, "/models", c.form); w.Code != c.code {
			t.Errorf("%s %v: %d, want %d", c.who, c.form, w.Code, c.code)
		}
	}
	if _, ok := keys.values["garrison/llm-groq"]; ok {
		t.Error("a refused provider left its key")
	}

	// Deleting a key.
	if w := post(s, "alice", origin, "/models", url.Values{"action": {"delete"}, "name": {"openrouter"}}); w.Code != 303 {
		t.Errorf("delete: %d", w.Code)
	}
	if _, ok := keys.values["garrison/llm-openrouter"]; ok {
		t.Error("not deleted")
	}
}
