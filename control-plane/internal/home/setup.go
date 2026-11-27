package home

import (
	"context"
	"crypto/subtle"
	"fmt"
	"log"
	"net/http"
	"net/mail"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"garrison/control-plane/internal/models"
)

// The setup completes an installation in the browser, as its first steps:
// the first root admin, the LLM providers, each tested with its key, and the
// default model, tested with a question. It is open, without signing in,
// only to whoever has the link the installer printed (its token, a file
// only root reads on the machine), and only until it is complete: a root
// admin exists, and garrison's config has llm/providers.yaml. Its route
// has no sign-in in front of it, since nobody could sign in yet.

// Setup is what the setup needs of Keycloak and garrison's config.
type Setup interface {
	// RootAdmins are the people who are root admins, not the platform's
	// own account.
	RootAdmins(ctx context.Context) ([]string, error)
	AddRootAdmin(ctx context.Context, username, email, password string) error
	// Configured tells whether garrison's config has llm/providers.yaml.
	Configured(ctx context.Context) (bool, error)
	// Save writes llm/providers.yaml and llm/models.yaml to garrison's
	// config, and each provider's key, for person.
	Save(ctx context.Context, person string, providers, catalog []byte, keys map[string]string) error
}

// setupCookie holds the token in the browser that opened the link.
const setupCookie = "garrison-setup"

// setupDraft is what the setup holds until it is saved: the providers
// tested so far, with their keys and models, and the root admin made.
type setupDraft struct {
	mu        sync.Mutex
	admin     string
	providers []draftProvider
}

type draftProvider struct {
	Name, URL string
	key       string
	Models    []string
}

// setupView is the setup page.
type setupView struct {
	Step      int // 1 the root admin, 2 the providers, 3 the default
	Admins    []string
	Providers []draftProvider
	Default   string
	Tried     string // the default's answer, once tested
	Name, URL string // the provider form's, after an error
	Notice    string
	Error     string
	Domain    string
}

// personName is a username the setup takes for the first root admin.
var personName = regexp.MustCompile(`^[a-z][a-z0-9._-]{1,38}[a-z0-9]$`)

// setupDone tells whether the setup is complete.
func (s *Server) setupDone(ctx context.Context) (bool, error) {
	admins, err := s.Setup.RootAdmins(ctx)
	if err != nil {
		return false, err
	}
	ok, err := s.Setup.Configured(ctx)
	return ok && len(admins) > 0, err
}

// setupToken is the installation's setup token, "" if there is none.
func (s *Server) setupToken() string {
	if s.SetupTokenFile == "" {
		return ""
	}
	raw, err := os.ReadFile(s.SetupTokenFile)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}

// setupAllowed tells whether the request carries the setup token, in the
// link (?token=) or in the cookie that link set.
func (s *Server) setupAllowed(r *http.Request) bool {
	want := s.setupToken()
	if len(want) < 16 {
		return false
	}
	have := r.URL.Query().Get("token")
	if have == "" {
		if c, err := r.Cookie(setupCookie); err == nil {
			have = c.Value
		}
	}
	return subtle.ConstantTimeCompare([]byte(have), []byte(want)) == 1
}

func (s *Server) setupGate(w http.ResponseWriter, r *http.Request) bool {
	if s.Setup == nil {
		http.NotFound(w, r)
		return false
	}
	done, err := s.setupDone(r.Context())
	if err != nil {
		log.Printf("home: setup: %v", err)
		http.Error(w, "cannot tell whether the setup is complete right now; try again in a moment", http.StatusServiceUnavailable)
		return false
	}
	if done {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return false
	}
	if !s.setupAllowed(r) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusForbidden)
		s.renderSetup(w, setupView{Error: "Open the setup link the installer printed. On the machine, it is in /etc/garrison/setup-link (sudo cat /etc/garrison/setup-link)."})
		return false
	}
	if t := r.URL.Query().Get("token"); t != "" {
		http.SetCookie(w, &http.Cookie{Name: setupCookie, Value: t, Path: "/setup", HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode, MaxAge: 86400})
		http.Redirect(w, r, "/setup", http.StatusSeeOther)
		return false
	}
	return true
}

func (s *Server) setupView(ctx context.Context) (setupView, error) {
	v := setupView{Domain: s.Domain}
	admins, err := s.Setup.RootAdmins(ctx)
	if err != nil {
		return v, err
	}
	configured, err := s.Setup.Configured(ctx)
	if err != nil {
		return v, err
	}
	d := &s.draft
	d.mu.Lock()
	defer d.mu.Unlock()
	v.Admins = admins
	v.Providers = slices.Clone(d.providers)
	switch {
	case len(admins) == 0:
		v.Step = 1
	case configured:
		v.Step = 0 // nothing left: setupGate sends it to the dashboard
	default:
		v.Step = 2
	}
	return v, nil
}

func (s *Server) setupPage(w http.ResponseWriter, r *http.Request) {
	if !s.setupGate(w, r) {
		return
	}
	v, err := s.setupView(r.Context())
	if err != nil {
		log.Printf("home: setup: %v", err)
		http.Error(w, "the setup cannot be read right now; try again", http.StatusServiceUnavailable)
		return
	}
	v.Notice = r.URL.Query().Get("notice")
	s.renderSetup(w, v)
}

// setupPost takes the setup's forms: action=admin (username, email,
// password, again), provider (name, url, key: tested, then kept),
// remove (name), try (default: asks the model a question), save (default).
func (s *Server) setupPost(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Origin") != "https://"+s.Domain {
		http.Error(w, "this form is only taken from the setup's own page", http.StatusForbidden)
		return
	}
	if !s.setupGate(w, r) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "the form is too long", http.StatusRequestEntityTooLarge)
		return
	}
	f := r.PostForm
	ctx := r.Context()
	v, err := s.setupView(ctx)
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	fail := func(status int, msg string) {
		v.Error = msg
		v.Name, v.URL = strings.TrimSpace(f.Get("name")), strings.TrimSpace(f.Get("url"))
		v.Default = f.Get("default")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(status)
		s.renderSetup(w, v)
	}
	done := func(notice string) {
		http.Redirect(w, r, "/setup?notice="+url.QueryEscape(notice), http.StatusSeeOther)
	}
	d := &s.draft
	switch f.Get("action") {
	case "admin":
		if v.Step != 1 {
			fail(http.StatusConflict, "There is a root admin already.")
			return
		}
		name := strings.ToLower(strings.TrimSpace(f.Get("username")))
		email := strings.TrimSpace(f.Get("email"))
		pw := f.Get("password")
		switch {
		case !personName.MatchString(name) || strings.EqualFold(name, s.PlatformUser):
			fail(http.StatusBadRequest, "A username is 3 to 40 lowercase letters, digits, dots, dashes or underscores, starting with a letter, and not "+s.PlatformUser+", the platform's own account.")
			return
		case !validEmail(email):
			fail(http.StatusBadRequest, "That is not an email address.")
			return
		case len(pw) < 12:
			fail(http.StatusBadRequest, "The password needs at least 12 characters.")
			return
		case pw != f.Get("again"):
			fail(http.StatusBadRequest, "The two passwords differ.")
			return
		}
		if err := s.Setup.AddRootAdmin(ctx, name, email, pw); err != nil {
			log.Printf("home: setup: root admin %s: %v", name, err)
			fail(http.StatusBadRequest, "Keycloak did not take it: "+err.Error())
			return
		}
		d.mu.Lock()
		d.admin = name
		d.mu.Unlock()
		log.Printf("home: setup: %s is the first root admin", name)
		done(name + " is a root admin. Now add the LLM providers.")
	case "provider":
		if v.Step != 2 {
			fail(http.StatusConflict, "Make the root admin first.")
			return
		}
		apiURL, key := strings.TrimSpace(f.Get("url")), strings.TrimSpace(f.Get("key"))
		name := strings.TrimSpace(f.Get("name"))
		if name == "" {
			name = models.ProviderName(apiURL)
		}
		if err := models.CheckProvider(name, apiURL, nil); err != nil {
			fail(http.StatusBadRequest, strings.TrimPrefix(err.Error(), models.ErrInvalidProvider.Error()+": "))
			return
		}
		tctx, cancel := context.WithTimeout(ctx, 90*time.Second)
		ids, err := models.List(tctx, s.client(), apiURL, key)
		if err == nil {
			err = models.CheckKey(tctx, s.client(), apiURL, key, ids)
		}
		cancel()
		if err != nil {
			fail(http.StatusBadRequest, "The test failed: "+err.Error()+". Check the URL and the key.")
			return
		}
		d.mu.Lock()
		d.providers = slices.DeleteFunc(d.providers, func(p draftProvider) bool { return p.Name == name })
		d.providers = append(d.providers, draftProvider{Name: name, URL: apiURL, key: key, Models: ids})
		d.mu.Unlock()
		done(fmt.Sprintf("%s works: it offers %d models.", name, len(ids)))
	case "remove":
		name := f.Get("name")
		d.mu.Lock()
		d.providers = slices.DeleteFunc(d.providers, func(p draftProvider) bool { return p.Name == name })
		d.mu.Unlock()
		done("Removed " + name + ".")
	case "try", "save":
		def := f.Get("default")
		prov, model, ok := strings.Cut(def, "/")
		d.mu.Lock()
		i := slices.IndexFunc(d.providers, func(p draftProvider) bool { return p.Name == prov })
		var p draftProvider
		if i >= 0 {
			p = d.providers[i]
		}
		d.mu.Unlock()
		if !ok || i < 0 || !slices.Contains(p.Models, model) {
			fail(http.StatusBadRequest, "Choose the default among the models of the providers tested.")
			return
		}
		tctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
		answer, err := models.Try(tctx, s.client(), p.URL, p.key, model)
		cancel()
		if err != nil {
			fail(http.StatusBadRequest, "The test failed: "+err.Error())
			return
		}
		if f.Get("action") == "try" {
			v.Default, v.Tried = def, answer
			s.renderSetup(w, v)
			return
		}
		if err := s.saveSetup(ctx, def); err != nil {
			log.Printf("home: setup: save: %v", err)
			fail(http.StatusServiceUnavailable, "It could not be saved: "+err.Error())
			return
		}
		http.SetCookie(w, &http.Cookie{Name: setupCookie, Value: "", Path: "/setup", MaxAge: -1, HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode})
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		s.renderSetup(w, setupView{Step: 4, Default: def, Admins: v.Admins, Domain: s.Domain, Tried: answer})
	default:
		http.Error(w, "unknown action", http.StatusBadRequest)
	}
}

// saveSetup writes the providers tested, with def as the default.
func (s *Server) saveSetup(ctx context.Context, def string) error {
	d := &s.draft
	d.mu.Lock()
	urls, lists, keys := map[string]string{}, map[string][]string{}, map[string]string{}
	for _, p := range d.providers {
		urls[p.Name], lists[p.Name] = p.URL, p.Models
		if p.key != "" {
			keys[p.Name] = p.key
		}
	}
	person := d.admin
	d.mu.Unlock()
	if person == "" {
		if admins, err := s.Setup.RootAdmins(ctx); err == nil && len(admins) > 0 {
			person = admins[0]
		}
	}
	rawP, rawM, err := models.Seeded(urls, lists, def, s.now().Format(time.DateOnly))
	if err != nil {
		return err
	}
	if err := s.Setup.Save(ctx, person, rawP, rawM, keys); err != nil {
		return err
	}
	d.mu.Lock()
	d.providers = nil
	d.mu.Unlock()
	log.Printf("home: setup: complete, default %s, by %s", def, person)
	return nil
}

func (s *Server) client() *http.Client {
	if s.HTTP != nil {
		return s.HTTP
	}
	return &http.Client{Timeout: 4 * time.Minute}
}

func (s *Server) renderSetup(w http.ResponseWriter, v setupView) {
	if v.Domain == "" {
		v.Domain = s.Domain
	}
	w.Header().Set("Cache-Control", "no-store")
	// same-origin: the link's token never leaves for another site, and the
	// form's own request still says where it comes from (no-referrer makes
	// browsers send Origin: null, which the form's check refuses).
	w.Header().Set("Referrer-Policy", "same-origin")
	if err := page.ExecuteTemplate(w, "setup", v); err != nil {
		log.Printf("home: setup page: %v", err)
	}
}

func validEmail(s string) bool {
	a, err := mail.ParseAddress(s)
	return err == nil && a.Address == s && strings.Contains(s[strings.LastIndex(s, "@"):], ".")
}
