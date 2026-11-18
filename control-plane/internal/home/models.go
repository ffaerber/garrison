package home

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"garrison/control-plane/internal/changes"
	"garrison/control-plane/internal/events"
	"garrison/control-plane/internal/models"
	"garrison/control-plane/internal/secrets"
)

// Providers is what the Models page needs of garrison's config.
type Providers interface {
	// Providers are llm/providers.yaml and llm/models.yaml on main; ok is
	// false while there are none.
	Providers(ctx context.Context) (models.Providers, models.Catalog, bool, error)
	// ProposeProvider proposes providers.yaml with the provider added, as
	// person, and returns the pull request's number. A name, URL or pattern
	// that cannot be one is models.ErrInvalidProvider; someone who may not,
	// changes.ErrForbidden.
	ProposeProvider(ctx context.Context, person, name, apiURL string, include []string) (int, error)
}

// providerRow is one provider on the Models page.
type providerRow struct {
	Name, URL      string
	Include        []string
	Key            bool // a key is stored here (else it may be on the machine)
	Enabled, Total int  // of its models in models.yaml
	Default        bool // the default model is one of its
}

// modelsView is the Models page.
type modelsView struct {
	Providers []providerRow
	Default   string
	None      bool   // garrison's config has no providers.yaml yet
	Provider  string // the provider whose key the form is for, from ?provider=
	Name, URL string // the form's, after an error
	Include   string
	Notice    string
	Error     string
	ModelsURL string // models.yaml in Gitea, where models are turned on
}

func (s *Server) modelsView(ctx context.Context) (*modelsView, error) {
	v := &modelsView{ModelsURL: fmt.Sprintf("https://git.%s/%s/%s/src/branch/main/%s", s.Domain, models.Namespace, models.Repo, models.ModelsFile)}
	p, catalog, ok, err := s.Providers.Providers(ctx)
	if err != nil {
		return nil, err
	}
	if !ok {
		v.None = true
		return v, nil
	}
	stored, err := s.Secrets.Names(ctx, secrets.PlatformNamespace)
	if err != nil {
		return nil, err
	}
	v.Default = p.Default
	def, _, _ := p.Resolve("")
	for _, name := range slices.Sorted(func(yield func(string) bool) {
		for k := range p.Providers {
			if !yield(k) {
				return
			}
		}
	}) {
		row := providerRow{Name: name, URL: p.Providers[name].URL, Include: p.Providers[name].Include,
			Key: slices.Contains(stored, models.KeySecret(name)), Default: name == def}
		for id, m := range catalog {
			if strings.HasPrefix(id, name+"/") && m.Gone == "" {
				row.Total++
				if m.Enabled {
					row.Enabled++
				}
			}
		}
		v.Providers = append(v.Providers, row)
	}
	return v, nil
}

// modelsPage lists the LLM providers, for root admins.
func (s *Server) modelsPage(w http.ResponseWriter, r *http.Request, v events.Visibility) {
	if s.Providers == nil || s.Secrets == nil {
		http.NotFound(w, r)
		return
	}
	if !v.All {
		http.Error(w, "only root admins manage the LLM providers", http.StatusForbidden)
		return
	}
	view, err := s.modelsView(r.Context())
	if err != nil {
		log.Printf("home: models: %v", err)
		http.Error(w, "the providers cannot be listed right now; try again", http.StatusServiceUnavailable)
		return
	}
	q := r.URL.Query()
	if name := q.Get("provider"); models.ValidNewProvider(name) {
		view.Provider = name
		switch q.Get("done") {
		case "key":
			view.Notice = fmt.Sprintf("Saved the key of %s. The agents use it within a minute.", name)
		case "deleted":
			view.Notice = fmt.Sprintf("Deleted the key of %s. The agents stop using it within a minute.", name)
		}
	}
	s.render(w, "page", feedPage{Who: v.Person, Domain: s.Domain, All: v.All, Now: s.now(), Models: view})
}

// modelsPost adds a provider (action=add: name, url, key, include), sets
// the key of one (action=key: name, key) or deletes it (action=delete: name).
func (s *Server) modelsPost(w http.ResponseWriter, r *http.Request, v events.Visibility) {
	if s.Providers == nil || s.Secrets == nil {
		http.NotFound(w, r)
		return
	}
	if r.Header.Get("Origin") != "https://"+s.Domain {
		http.Error(w, "this form is only taken from the dashboard's own page", http.StatusForbidden)
		return
	}
	if !v.All {
		http.Error(w, "only root admins manage the LLM providers", http.StatusForbidden)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, secrets.MaxValue+16<<10)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "the key is too long", http.StatusRequestEntityTooLarge)
		return
	}
	f := r.PostForm
	name, action := strings.TrimSpace(f.Get("name")), f.Get("action")
	apiURL, key := strings.TrimSpace(f.Get("url")), strings.TrimSpace(f.Get("key"))
	var include []string
	for _, p := range strings.FieldsFunc(f.Get("include"), func(c rune) bool { return c == ',' || c == ' ' || c == '\n' }) {
		include = append(include, p)
	}
	fail := func(status int, err error) {
		view, verr := s.modelsView(r.Context())
		if verr != nil {
			http.Error(w, err.Error(), status)
			return
		}
		view.Error, view.Name, view.URL, view.Include, view.Provider = err.Error(), name, apiURL, f.Get("include"), name
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(status)
		s.render(w, "page", feedPage{Who: v.Person, Domain: s.Domain, All: v.All, Now: s.now(), Models: view})
	}
	failed := func(err error) {
		switch {
		case errors.Is(err, changes.ErrForbidden), errors.Is(err, secrets.ErrForbidden):
			fail(http.StatusForbidden, err)
		case errors.Is(err, models.ErrInvalidProvider), errors.Is(err, secrets.ErrInvalid):
			fail(http.StatusBadRequest, err)
		default:
			log.Printf("home: %s: models %s %s: %v", v.Person, action, name, err)
			fail(http.StatusServiceUnavailable, err)
		}
	}
	if !models.ValidNewProvider(name) {
		failed(fmt.Errorf("%w: a provider's name is lowercase letters, digits and dashes, at most %d, e.g. openrouter", models.ErrInvalidProvider, models.MaxNewProvider))
		return
	}
	p, _, _, err := s.Providers.Providers(r.Context())
	if err != nil {
		failed(err)
		return
	}
	_, exists := p.Providers[name]
	record := func(title string) {
		s.Events.Record(r.Context(), events.Event{Source: events.SourceHome, Kind: events.KindSecret, Status: events.StatusOK,
			Namespace: models.Namespace, Repo: models.Repo, Audience: events.AudienceMembers, Actor: v.Person, StartedBy: v.Person,
			Title: title, Link: "https://" + s.Domain + "/models?provider=" + url.QueryEscape(name)})
	}
	done := url.Values{"provider": {name}}
	switch action {
	case "add":
		if !exists {
			// Checked before the key is stored, so a refused provider
			// leaves no key behind.
			if err := models.CheckProvider(name, apiURL, include); err != nil {
				failed(err)
				return
			}
		}
		fallthrough
	case "key":
		if action == "key" && !exists {
			failed(fmt.Errorf("%w: %s has no provider %s; add it first", models.ErrInvalidProvider, models.ProvidersFile, name))
			return
		}
		if err := secrets.CheckValue(key); err != nil {
			failed(fmt.Errorf("%w: the API key is empty or not text", secrets.ErrInvalid))
			return
		}
		if err := s.Secrets.Set(r.Context(), secrets.PlatformNamespace, models.KeySecret(name), key, v.Person); err != nil {
			failed(err)
			return
		}
		record(fmt.Sprintf("%s set the API key of the LLM provider %s", v.Person, name))
		done.Set("done", "key")
		if !exists {
			n, err := s.Providers.ProposeProvider(r.Context(), v.Person, name, apiURL, include)
			if err != nil {
				failed(fmt.Errorf("the key of %s is saved, but %s could not be proposed: %w", name, models.ProvidersFile, err))
				return
			}
			http.Redirect(w, r, changes.Path(models.Namespace, models.Repo, n), http.StatusSeeOther)
			return
		}
	case "delete":
		if err := s.Secrets.Delete(r.Context(), secrets.PlatformNamespace, models.KeySecret(name), v.Person); err != nil {
			failed(err)
			return
		}
		record(fmt.Sprintf("%s deleted the API key of the LLM provider %s", v.Person, name))
		done.Set("done", "deleted")
	default:
		http.Error(w, "unknown action", http.StatusBadRequest)
		return
	}
	http.Redirect(w, r, "/models?"+done.Encode(), http.StatusSeeOther)
}
