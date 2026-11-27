package home

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type fakeSetup struct {
	admins     []string
	configured bool
	files      map[string]string
	keys       map[string]string
	by         string
}

func (f *fakeSetup) RootAdmins(context.Context) ([]string, error) { return f.admins, nil }
func (f *fakeSetup) AddRootAdmin(_ context.Context, name, email, pw string) error {
	f.admins = append(f.admins, name)
	return nil
}
func (f *fakeSetup) Configured(context.Context) (bool, error) { return f.configured, nil }
func (f *fakeSetup) Save(_ context.Context, person string, p, c []byte, keys map[string]string) error {
	f.files = map[string]string{"providers": string(p), "models": string(c)}
	f.keys, f.by, f.configured = keys, person, true
	return nil
}

// setupReq is a request to the setup, with the cookie the link set.
func setupReq(s *Server, method, path, cookie string, form url.Values) *httptest.ResponseRecorder {
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	r := httptest.NewRequest(method, path, body)
	if form != nil {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set("Origin", "https://home.test")
	}
	if cookie != "" {
		r.AddCookie(&http.Cookie{Name: setupCookie, Value: cookie})
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w
}

func TestTheSetup(t *testing.T) {
	// Two providers: gpu-box on the LAN, without a key, and a cloud that
	// takes only its key.
	llm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The cloud lists its models for anyone, as Ollama's does, and
		// refuses a wrong key only when a model is asked.
		cloud := strings.HasPrefix(r.URL.Path, "/cloud/")
		if cloud && strings.HasSuffix(r.URL.Path, "/chat/completions") && r.Header.Get("Authorization") != "Bearer sk-cloud" {
			http.Error(w, `{"error":"bad key"}`, http.StatusUnauthorized)
			return
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/v1/models") && cloud:
			io.WriteString(w, `{"data":[{"id":"glm-5.3"},{"id":"kimi-k3"}]}`)
		case strings.HasSuffix(r.URL.Path, "/v1/models"):
			io.WriteString(w, `{"data":[{"id":"qwen3.8-27b"}]}`)
		case strings.HasSuffix(r.URL.Path, "/v1/chat/completions"):
			var req struct{ Model string }
			json.NewDecoder(r.Body).Decode(&req)
			io.WriteString(w, `{"choices":[{"message":{"content":"Ready."}}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer llm.Close()

	token := "0123456789abcdef0123456789abcdef"
	tokenFile := filepath.Join(t.TempDir(), "setup-token")
	os.WriteFile(tokenFile, []byte(token+"\n"), 0o600)
	s, _ := server()
	f := &fakeSetup{}
	s.Setup, s.SetupTokenFile, s.PlatformUser, s.HTTP = f, tokenFile, "admin", llm.Client()

	// Without the token, or with a wrong one: nothing but how to find it.
	for _, w := range []*httptest.ResponseRecorder{
		setupReq(s, "GET", "/setup", "", nil),
		setupReq(s, "GET", "/setup?token=wrong", "", nil),
		setupReq(s, "POST", "/setup", "wrong", url.Values{"action": {"admin"}, "username": {"eve"}}),
	} {
		if w.Code != 403 || !strings.Contains(w.Body.String(), "/etc/garrison/setup-link") || strings.Contains(w.Body.String(), "Make the root admin") {
			t.Errorf("without the token: %d", w.Code)
		}
	}
	if len(f.admins) != 0 {
		t.Fatal("a root admin was made without the token")
	}
	// The link sets a cookie for /setup only, and goes on without the token in it.
	w := setupReq(s, "GET", "/setup?token="+token, "", nil)
	c := w.Result().Cookies()
	if w.Code != 303 || w.Header().Get("Location") != "/setup" || len(c) != 1 || c[0].Value != token || c[0].Path != "/setup" || !c[0].HttpOnly || !c[0].Secure {
		t.Fatalf("link: %d %s %v", w.Code, w.Header().Get("Location"), c)
	}

	// 1. The root admin.
	if body := setupReq(s, "GET", "/setup", token, nil).Body.String(); !strings.Contains(body, "Make the root admin") {
		t.Fatalf("step 1:\n%s", body)
	}
	for _, bad := range []url.Values{
		{"action": {"admin"}, "username": {"admin"}, "email": {"a@b.test"}, "password": {"long enough pw"}, "again": {"long enough pw"}},
		{"action": {"admin"}, "username": {"felix"}, "email": {"nope"}, "password": {"long enough pw"}, "again": {"long enough pw"}},
		{"action": {"admin"}, "username": {"felix"}, "email": {"f@home.test"}, "password": {"short"}, "again": {"short"}},
		{"action": {"admin"}, "username": {"felix"}, "email": {"f@home.test"}, "password": {"long enough pw"}, "again": {"other"}},
	} {
		if w := setupReq(s, "POST", "/setup", token, bad); w.Code != 400 {
			t.Errorf("%v: %d", bad, w.Code)
		}
	}
	if w := setupReq(s, "POST", "/setup", token, url.Values{"action": {"admin"}, "username": {"felix"}, "email": {"f@home.test"}, "password": {"long enough pw"}, "again": {"long enough pw"}}); w.Code != 303 {
		t.Fatalf("admin: %d\n%s", w.Code, w.Body)
	}
	if len(f.admins) != 1 || f.admins[0] != "felix" {
		t.Fatalf("admins: %v", f.admins)
	}

	// 2. The providers, each tested: a wrong key is refused.
	if w := setupReq(s, "POST", "/setup", token, url.Values{"action": {"provider"}, "name": {"ollama-cloud"}, "url": {llm.URL + "/cloud/v1"}, "key": {"wrong"}}); w.Code != 400 || !strings.Contains(w.Body.String(), "refuses the key") {
		t.Errorf("wrong key: %d", w.Code)
	}
	for _, form := range []url.Values{
		{"action": {"provider"}, "name": {"gpu-box"}, "url": {llm.URL + "/v1"}},
		{"action": {"provider"}, "name": {"ollama-cloud"}, "url": {llm.URL + "/cloud/v1"}, "key": {"sk-cloud"}},
	} {
		if w := setupReq(s, "POST", "/setup", token, form); w.Code != 303 || !strings.Contains(w.Header().Get("Location"), "works") {
			t.Fatalf("%v: %d %s\n%s", form, w.Code, w.Header().Get("Location"), w.Body)
		}
	}
	body := setupReq(s, "GET", "/setup", token, nil).Body.String()
	for _, want := range []string{"<code>gpu-box</code>", "<code>ollama-cloud</code>", `value="gpu-box/qwen3.8-27b"`, `value="ollama-cloud/glm-5.3"`, "Test and save"} {
		if !strings.Contains(body, want) {
			t.Errorf("no %q in step 3", want)
		}
	}
	if strings.Contains(body, "sk-cloud") {
		t.Error("the page shows a key")
	}

	// 3. The default, tested; a model no provider tested is refused.
	if w := setupReq(s, "POST", "/setup", token, url.Values{"action": {"save"}, "default": {"gpu-box/gpt-9"}}); w.Code != 400 {
		t.Errorf("unknown default: %d", w.Code)
	}
	if w := setupReq(s, "POST", "/setup", token, url.Values{"action": {"try"}, "default": {"gpu-box/qwen3.8-27b"}}); w.Code != 200 || !strings.Contains(w.Body.String(), "Ready.") {
		t.Errorf("try: %d", w.Code)
	}
	w = setupReq(s, "POST", "/setup", token, url.Values{"action": {"save"}, "default": {"gpu-box/qwen3.8-27b"}})
	if w.Code != 200 || !strings.Contains(w.Body.String(), "Garrison is ready") {
		t.Fatalf("save: %d\n%s", w.Code, w.Body)
	}
	if !strings.Contains(f.files["providers"], `default: "gpu-box/qwen3.8-27b"`) || !strings.Contains(f.files["providers"], "ollama-cloud:\n    url: ") ||
		!strings.Contains(f.files["models"], `"gpu-box/qwen3.8-27b": {enabled: true`) || !strings.Contains(f.files["models"], `"ollama-cloud/glm-5.3": {enabled: false`) {
		t.Errorf("saved:\n%s\n%s", f.files["providers"], f.files["models"])
	}
	if len(f.keys) != 1 || f.keys["ollama-cloud"] != "sk-cloud" || f.by != "felix" {
		t.Errorf("keys %v by %s", f.keys, f.by)
	}

	// Done: the setup is closed, even with the token.
	if w := setupReq(s, "GET", "/setup?token="+token, "", nil); w.Code != 303 || w.Header().Get("Location") != "/" {
		t.Errorf("after: %d %s", w.Code, w.Header().Get("Location"))
	}
	if w := setupReq(s, "POST", "/setup", token, url.Values{"action": {"admin"}, "username": {"eve"}, "email": {"e@home.test"}, "password": {"long enough pw"}, "again": {"long enough pw"}}); w.Code == 303 && len(f.admins) > 1 {
		t.Error("another root admin was made after the setup")
	}
}

func TestNoSetupWithoutAToken(t *testing.T) {
	s, _ := server()
	s.Setup = &fakeSetup{}
	s.SetupTokenFile = filepath.Join(t.TempDir(), "none")
	if w := setupReq(s, "GET", "/setup?token=", "", nil); w.Code != 403 {
		t.Errorf("an empty token opened it: %d", w.Code)
	}
	s.Setup = nil
	if w := setupReq(s, "GET", "/setup", "", nil); w.Code != 404 {
		t.Errorf("without a setup: %d", w.Code)
	}
}
