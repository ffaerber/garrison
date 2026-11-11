// Package controlplane is the web control plane: people sign in through
// Keycloak, see the stacks and the changes waiting for approval, and talk to
// the operator agent.
package controlplane

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

//go:embed ui
var uiFiles embed.FS

const (
	sessionCookie = "cp_session"
	loginCookie   = "cp_login"
	sessionTTL    = 12 * time.Hour
	adminGroup    = "root-admins"
	maxHistory    = 40
)

type Config struct {
	PublicURL    string // e.g. https://controlplane.example.com
	Issuer       string // e.g. https://keycloak.example.com/realms/homecloud
	ClientID     string
	ClientSecret string
	HTTP         *http.Client // trusts the installation's certificate; used for Keycloak
	Agent        *Agent
	Gitea        *Gitea
}

type session struct {
	person  Person
	idToken string
	expires time.Time
	mu      sync.Mutex // one agent turn at a time
	history []Message
}

type pendingLogin struct {
	nonce, verifier, next string
	expires               time.Time
}

type Server struct {
	cfg      Config
	oauth    oauth2.Config
	verifier *oidc.IDTokenVerifier
	endURL   string

	mu       sync.Mutex
	sessions map[string]*session
	logins   map[string]pendingLogin
}

// New discovers Keycloak and returns the server. Discovery is retried, since
// Keycloak may still be starting.
func New(ctx context.Context, cfg Config) (*Server, error) {
	ctx = oidc.ClientContext(ctx, cfg.HTTP)
	var provider *oidc.Provider
	var err error
	for i := 0; i < 60; i++ {
		if provider, err = oidc.NewProvider(ctx, cfg.Issuer); err == nil {
			break
		}
		log.Printf("waiting for Keycloak: %v", err)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
	if err != nil {
		return nil, err
	}
	var meta struct {
		EndSession string `json:"end_session_endpoint"`
	}
	provider.Claims(&meta)
	return &Server{
		cfg: cfg,
		oauth: oauth2.Config{
			ClientID: cfg.ClientID, ClientSecret: cfg.ClientSecret,
			Endpoint:    provider.Endpoint(),
			RedirectURL: strings.TrimSuffix(cfg.PublicURL, "/") + "/callback",
			Scopes:      []string{oidc.ScopeOpenID, "profile", "email"},
		},
		verifier: provider.Verifier(&oidc.Config{ClientID: cfg.ClientID}),
		endURL:   meta.EndSession,
		sessions: map[string]*session{},
		logins:   map[string]pendingLogin{},
	}, nil
}

func randomID() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	static, _ := fs.Sub(uiFiles, "ui")
	mux.HandleFunc("GET /login", s.login)
	mux.HandleFunc("GET /callback", s.callback)
	mux.HandleFunc("GET /logout", s.logout)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	mux.Handle("GET /{$}", s.page(http.FileServerFS(static)))
	mux.Handle("GET /app.js", http.FileServerFS(static))
	mux.Handle("GET /app.css", http.FileServerFS(static))
	mux.HandleFunc("GET /api/me", s.api(s.me))
	mux.HandleFunc("GET /api/stacks", s.api(s.stacks))
	mux.HandleFunc("GET /api/changes", s.api(s.changes))
	mux.HandleFunc("POST /api/chat", s.api(s.chat))
	mux.HandleFunc("POST /api/chat/reset", s.api(s.reset))
	mux.HandleFunc("POST /api/changes/{repo}/{number}/{action}", s.api(s.decide))
	return securityHeaders(mux)
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; frame-ancestors 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		next.ServeHTTP(w, r)
	})
}

func (s *Server) current(r *http.Request) *session {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sess := s.sessions[c.Value]
	if sess == nil || time.Now().After(sess.expires) {
		delete(s.sessions, c.Value)
		return nil
	}
	return sess
}

// page serves the UI to people who are signed in and sends others to log in.
func (s *Server) page(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.current(r) == nil {
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

type apiFunc func(w http.ResponseWriter, r *http.Request, sess *session) (any, error)

// api requires a session, and for changes, a request from this origin.
func (s *Server) api(f apiFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess := s.current(r)
		if sess == nil {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "not signed in"})
			return
		}
		if r.Method != http.MethodGet {
			origin := r.Header.Get("Origin")
			if origin == "" || origin != strings.TrimSuffix(s.cfg.PublicURL, "/") || !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
				writeJSON(w, http.StatusForbidden, map[string]string{"error": "cross-site request refused"})
				return
			}
		}
		out, err := f(w, r, sess)
		switch {
		case errors.Is(err, ErrInvalid):
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		case errors.Is(err, ErrForbidden):
			writeJSON(w, http.StatusForbidden, map[string]string{"error": sess.person.Name + " may not do this"})
		case err != nil:
			log.Printf("%s %s for %s: %v", r.Method, r.URL.Path, sess.person.Name, err)
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		default:
			writeJSON(w, http.StatusOK, out)
		}
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// ---------- sign-in ----------

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	state, nonce, verifier := randomID(), randomID(), oauth2.GenerateVerifier()
	s.mu.Lock()
	for k, v := range s.logins {
		if time.Now().After(v.expires) {
			delete(s.logins, k)
		}
	}
	s.logins[state] = pendingLogin{nonce: nonce, verifier: verifier, expires: time.Now().Add(10 * time.Minute)}
	s.mu.Unlock()
	// Ties the callback to this browser.
	http.SetCookie(w, &http.Cookie{Name: loginCookie, Value: hash(state), Path: "/callback", MaxAge: 600, HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode})
	http.Redirect(w, r, s.oauth.AuthCodeURL(state, oidc.Nonce(nonce), oauth2.S256ChallengeOption(verifier)), http.StatusFound)
}

func hash(s string) string {
	h := sha256.Sum256([]byte(s))
	return base64.RawURLEncoding.EncodeToString(h[:])
}

func (s *Server) callback(w http.ResponseWriter, r *http.Request) {
	state := r.URL.Query().Get("state")
	c, err := r.Cookie(loginCookie)
	if err != nil || subtle.ConstantTimeCompare([]byte(c.Value), []byte(hash(state))) != 1 {
		http.Error(w, "this sign-in was not started in this browser; please try again", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	pending, ok := s.logins[state]
	delete(s.logins, state)
	s.mu.Unlock()
	if !ok || time.Now().After(pending.expires) {
		http.Error(w, "sign-in expired; please try again", http.StatusBadRequest)
		return
	}
	ctx := oidc.ClientContext(r.Context(), s.cfg.HTTP)
	token, err := s.oauth.Exchange(ctx, r.URL.Query().Get("code"), oauth2.VerifierOption(pending.verifier))
	if err != nil {
		log.Printf("sign-in: %v", err)
		http.Error(w, "sign-in failed", http.StatusBadGateway)
		return
	}
	raw, _ := token.Extra("id_token").(string)
	idToken, err := s.verifier.Verify(ctx, raw)
	if err != nil || idToken.Nonce != pending.nonce {
		log.Printf("sign-in: invalid ID token: %v", err)
		http.Error(w, "sign-in failed", http.StatusBadGateway)
		return
	}
	var claims struct {
		Username string   `json:"preferred_username"`
		Email    string   `json:"email"`
		Groups   []string `json:"groups"`
	}
	if err := idToken.Claims(&claims); err != nil || !userName.MatchString(claims.Username) {
		http.Error(w, "sign-in failed: no usable username", http.StatusBadGateway)
		return
	}
	id := randomID()
	s.mu.Lock()
	s.sessions[id] = &session{
		person:  Person{Name: claims.Username, Email: claims.Email, Admin: slices.Contains(claims.Groups, adminGroup)},
		idToken: raw,
		expires: time.Now().Add(sessionTTL),
	}
	s.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: loginCookie, Path: "/callback", MaxAge: -1})
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: id, Path: "/", MaxAge: int(sessionTTL.Seconds()), HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode})
	log.Printf("%s signed in", claims.Username)
	http.Redirect(w, r, "/", http.StatusFound)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	target := "/"
	if c, err := r.Cookie(sessionCookie); err == nil {
		s.mu.Lock()
		if sess := s.sessions[c.Value]; sess != nil && s.endURL != "" {
			q := url.Values{"id_token_hint": {sess.idToken}, "post_logout_redirect_uri": {strings.TrimSuffix(s.cfg.PublicURL, "/") + "/"}}
			target = s.endURL + "?" + q.Encode()
		}
		delete(s.sessions, c.Value)
		s.mu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Path: "/", MaxAge: -1})
	http.Redirect(w, r, target, http.StatusFound)
}

// ---------- API ----------

func (s *Server) me(w http.ResponseWriter, r *http.Request, sess *session) (any, error) {
	return sess.person, nil
}

func (s *Server) stacks(w http.ResponseWriter, r *http.Request, sess *session) (any, error) {
	return s.cfg.Agent.Stacks(r.Context())
}

func (s *Server) changes(w http.ResponseWriter, r *http.Request, sess *session) (any, error) {
	return s.cfg.Gitea.Changes(r.Context(), sess.person.Name)
}

func (s *Server) chat(w http.ResponseWriter, r *http.Request, sess *session) (any, error) {
	var in struct {
		Message string `json:"message"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&in); err != nil || strings.TrimSpace(in.Message) == "" {
		return nil, fmt.Errorf("%w: empty message", ErrInvalid)
	}
	sess.mu.Lock()
	defer sess.mu.Unlock()
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	reply, steps, history, err := s.cfg.Agent.Ask(ctx, sess.person, sess.history, in.Message)
	if err != nil {
		return nil, err
	}
	if len(history) > maxHistory {
		history = history[len(history)-maxHistory:]
		// Never start with a tool answer whose call was cut off.
		for len(history) > 0 && history[0].Role != "user" {
			history = history[1:]
		}
	}
	sess.history = history
	return map[string]any{"reply": reply, "steps": steps}, nil
}

func (s *Server) reset(w http.ResponseWriter, r *http.Request, sess *session) (any, error) {
	sess.mu.Lock()
	sess.history = nil
	sess.mu.Unlock()
	return map[string]bool{"ok": true}, nil
}

func (s *Server) decide(w http.ResponseWriter, r *http.Request, sess *session) (any, error) {
	repo := r.PathValue("repo")
	number, err := strconv.Atoi(r.PathValue("number"))
	if err != nil || number <= 0 {
		return nil, fmt.Errorf("%w: change number", ErrInvalid)
	}
	switch r.PathValue("action") {
	case "approve":
		err = s.cfg.Gitea.Approve(r.Context(), sess.person.Name, repo, number)
	case "reject":
		err = s.cfg.Gitea.Reject(r.Context(), sess.person.Name, repo, number)
	default:
		return nil, fmt.Errorf("%w: unknown action", ErrInvalid)
	}
	if err != nil {
		return nil, err
	}
	log.Printf("%s: %s %s#%d", sess.person.Name, r.PathValue("action"), repo, number)
	return map[string]bool{"ok": true}, nil
}
