package gateway

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"
)

// Keycloak keeps the agents' accounts in Keycloak, so an agent can sign in
// where a person would: its browser shows the pages behind the sign-in as
// the agent, also in a run without a person. Each agent with an account in
// Gitea (see Members.accounts) has one of the same name in the realm
// garrison, e.g. agent-todo-designer, disabled when its Gitea account is
// locked.
//
// Nobody else signs in with them. Their passwords, and the secret of the
// client agents through which the gateway gets their tokens, are derived
// from the admin password, which the gateway holds anyway: there is no
// further secret to keep. The client's tokens name sso as an audience, so
// the sign-in in front of private environments takes them, as it takes
// Open WebUI's; the gateway's own MCP endpoint does not.
type Keycloak struct {
	URL      string // e.g. http://keycloak:8080, on the system network
	User     string // the master realm's admin
	Password string
	HTTP     *http.Client // nil: http.DefaultClient

	mu     sync.Mutex
	client bool               // the client agents is set up
	tokens map[string]kcToken // account -> its last token
	made   map[string]bool    // account -> enabled, as last set
}

type kcToken struct {
	raw     string
	expires time.Time
}

const (
	kcRealm  = "garrison"
	kcClient = "agents"
	// kcAudience is the login proxy's client: the sign-in in front of
	// private environments takes tokens that name it.
	kcAudience = "sso"
)

// derive is a value only the holder of the admin password can know.
func (k *Keycloak) derive(what string) string {
	m := hmac.New(sha256.New, []byte(k.Password))
	m.Write([]byte("garrison keycloak v1 " + what))
	return hex.EncodeToString(m.Sum(nil))
}

func (k *Keycloak) http() *http.Client {
	if k.HTTP != nil {
		return k.HTTP
	}
	return http.DefaultClient
}

// form posts a form to a token endpoint and decodes the answer.
func (k *Keycloak) form(ctx context.Context, realm string, v url.Values) (kcToken, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, k.URL+"/realms/"+realm+"/protocol/openid-connect/token", strings.NewReader(v.Encode()))
	if err != nil {
		return kcToken{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := k.http().Do(req)
	if err != nil {
		return kcToken{}, err
	}
	defer resp.Body.Close()
	var out struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
		Error       string `json:"error"`
		Description string `json:"error_description"`
	}
	json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out)
	if resp.StatusCode != http.StatusOK || out.AccessToken == "" {
		return kcToken{}, &kcError{status: resp.StatusCode, code: out.Error, msg: out.Description}
	}
	return kcToken{raw: out.AccessToken, expires: time.Now().Add(time.Duration(out.ExpiresIn) * time.Second)}, nil
}

type kcError struct {
	status    int
	code, msg string
}

func (e *kcError) Error() string {
	return fmt.Sprintf("keycloak: %d %s %s", e.status, e.code, e.msg)
}

// admin calls the admin API of the realm garrison as the master realm's
// admin. path starts with a slash; out may be nil.
func (k *Keycloak) admin(ctx context.Context, method, path string, body, out any) (int, error) {
	tok, err := k.form(ctx, "master", url.Values{
		"grant_type": {"password"}, "client_id": {"admin-cli"},
		"username": {k.User}, "password": {k.Password},
	})
	if err != nil {
		return 0, fmt.Errorf("sign in as the admin: %w", err)
	}
	var r io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		r = strings.NewReader(string(data))
	}
	req, err := http.NewRequestWithContext(ctx, method, k.URL+"/admin/realms/"+kcRealm+path, r)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+tok.raw)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := k.http().Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		return resp.StatusCode, fmt.Errorf("keycloak %s %s: %d %s", method, path, resp.StatusCode, strings.TrimSpace(string(data)))
	}
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return resp.StatusCode, err
		}
	}
	return resp.StatusCode, nil
}

// Email is the email address of the person signed in to Garrison as
// username, or "" for no such person.
func (k *Keycloak) Email(ctx context.Context, username string) (string, error) {
	var users []struct {
		Username string `json:"username"`
		Email    string `json:"email"`
	}
	if _, err := k.admin(ctx, http.MethodGet, "/users?exact=true&username="+url.QueryEscape(username), nil, &users); err != nil {
		return "", err
	}
	for _, u := range users {
		if strings.EqualFold(u.Username, username) {
			return u.Email, nil
		}
	}
	return "", nil
}

// setupClient makes the client agents, or brings it back to how it must
// be: confidential, password grants only, its secret derived, its tokens
// naming sso.
func (k *Keycloak) setupClient(ctx context.Context) error {
	k.mu.Lock()
	done := k.client
	k.mu.Unlock()
	if done {
		return nil
	}
	rep := map[string]any{
		"clientId": kcClient, "name": "Garrison's agents", "enabled": true,
		"publicClient": false, "secret": k.derive("client " + kcClient),
		"standardFlowEnabled": false, "implicitFlowEnabled": false,
		"directAccessGrantsEnabled": true, "serviceAccountsEnabled": false,
	}
	var found []struct {
		ID string `json:"id"`
	}
	if _, err := k.admin(ctx, http.MethodGet, "/clients?clientId="+kcClient, nil, &found); err != nil {
		return err
	}
	if len(found) == 0 {
		if _, err := k.admin(ctx, http.MethodPost, "/clients", rep, nil); err != nil {
			return err
		}
		if _, err := k.admin(ctx, http.MethodGet, "/clients?clientId="+kcClient, nil, &found); err != nil {
			return err
		}
		if len(found) == 0 {
			return errors.New("keycloak: the client agents is not there after making it")
		}
	} else if _, err := k.admin(ctx, http.MethodPut, "/clients/"+found[0].ID, rep, nil); err != nil {
		return err
	}
	var mappers []struct {
		Name string `json:"name"`
	}
	if _, err := k.admin(ctx, http.MethodGet, "/clients/"+found[0].ID+"/protocol-mappers/models", nil, &mappers); err != nil {
		return err
	}
	has := false
	for _, m := range mappers {
		has = has || m.Name == "audience-"+kcAudience
	}
	if !has {
		if _, err := k.admin(ctx, http.MethodPost, "/clients/"+found[0].ID+"/protocol-mappers/models", map[string]any{
			"name": "audience-" + kcAudience, "protocol": "openid-connect", "protocolMapper": "oidc-audience-mapper",
			"config": map[string]string{"included.client.audience": kcAudience, "access.token.claim": "true", "id.token.claim": "false"},
		}, nil); err != nil {
			return err
		}
	}
	k.mu.Lock()
	k.client = true
	k.mu.Unlock()
	return nil
}

type kcUser struct {
	ID      string `json:"id"`
	Enabled bool   `json:"enabled"`
}

func (k *Keycloak) user(ctx context.Context, account string) (*kcUser, error) {
	var found []kcUser
	if _, err := k.admin(ctx, http.MethodGet, "/users?exact=true&username="+url.QueryEscape(account), nil, &found); err != nil {
		return nil, err
	}
	if len(found) == 0 {
		return nil, nil
	}
	return &found[0], nil
}

// Keep makes the account of the agent id (e.g. todo/designer) when it is
// missing, and enables or disables it. It asks Keycloak again only when
// what it set last differs.
func (k *Keycloak) Keep(ctx context.Context, account, id string, enabled bool) error {
	k.mu.Lock()
	last, known := k.made[account]
	k.mu.Unlock()
	if known && last == enabled {
		return nil
	}
	u, err := k.user(ctx, account)
	if err != nil {
		return err
	}
	// The whole profile each time: Keycloak's user profile takes what a
	// request leaves out as removed.
	ns, name, _ := strings.Cut(id, "/")
	rep := map[string]any{
		"username": account, "enabled": enabled,
		"email": account + "@agents.invalid", "emailVerified": true,
		// Keycloak's names take no slash.
		"firstName": "Agent", "lastName": ns + " " + name,
		"attributes":      map[string][]string{"garrison-agent": {id}},
		"requiredActions": []string{},
	}
	switch {
	case u == nil && !enabled:
	case u == nil:
		rep["credentials"] = []map[string]any{{"type": "password", "value": k.derive("user " + account), "temporary": false}}
		if _, err := k.admin(ctx, http.MethodPost, "/users", rep, nil); err != nil {
			return err
		}
	case u.Enabled != enabled:
		if _, err := k.admin(ctx, http.MethodPut, "/users/"+u.ID, rep, nil); err != nil {
			return err
		}
	}
	k.mu.Lock()
	if k.made == nil {
		k.made = map[string]bool{}
	}
	k.made[account] = enabled
	if !enabled {
		delete(k.tokens, account)
	}
	k.mu.Unlock()
	return nil
}

// Token is an access token of the agent's account: through the client
// agents, with the account's derived password. A password that no longer
// works (someone changed it in Keycloak) is set back once.
func (k *Keycloak) Token(ctx context.Context, account, id string) (string, error) {
	k.mu.Lock()
	t, ok := k.tokens[account]
	enabled, known := k.made[account]
	locked := known && !enabled
	k.mu.Unlock()
	if locked {
		return "", fmt.Errorf("the account %s is locked: its agent is gone, switched off or has no grants or routines here", account)
	}
	if ok && time.Until(t.expires) > 30*time.Second {
		return t.raw, nil
	}
	if err := k.setupClient(ctx); err != nil {
		return "", err
	}
	if err := k.Keep(ctx, account, id, true); err != nil {
		return "", err
	}
	get := func() (kcToken, error) {
		return k.form(ctx, kcRealm, url.Values{
			"grant_type": {"password"}, "client_id": {kcClient}, "client_secret": {k.derive("client " + kcClient)},
			"username": {account}, "password": {k.derive("user " + account)}, "scope": {"openid"},
		})
	}
	t, err := get()
	var kerr *kcError
	if errors.As(err, &kerr) && kerr.code == "invalid_grant" {
		u, uerr := k.user(ctx, account)
		if uerr != nil || u == nil {
			return "", errors.Join(err, uerr)
		}
		if !u.Enabled {
			return "", fmt.Errorf("the account %s is disabled in Keycloak", account)
		}
		if _, perr := k.admin(ctx, http.MethodPut, "/users/"+u.ID+"/reset-password", map[string]any{
			"type": "password", "value": k.derive("user " + account), "temporary": false,
		}, nil); perr != nil {
			return "", perr
		}
		t, err = get()
	}
	if err != nil {
		return "", fmt.Errorf("sign in as %s: %w", account, err)
	}
	k.mu.Lock()
	if k.tokens == nil {
		k.tokens = map[string]kcToken{}
	}
	k.tokens[account] = t
	k.mu.Unlock()
	return t.raw, nil
}

// RootTeamGroup is the realm's group of root admins.
const RootTeamGroup = "root-admins"

func (k *Keycloak) rootGroup(ctx context.Context) (string, error) {
	var groups []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if _, err := k.admin(ctx, http.MethodGet, "/groups?exact=true&search="+RootTeamGroup, nil, &groups); err != nil {
		return "", err
	}
	for _, g := range groups {
		if g.Name == RootTeamGroup {
			return g.ID, nil
		}
	}
	return "", fmt.Errorf("keycloak: the realm %s has no group %s", kcRealm, RootTeamGroup)
}

// RootAdmins are the people of the group root-admins, by username, but for
// the platform's own account (k.User), which the installer makes.
func (k *Keycloak) RootAdmins(ctx context.Context) ([]string, error) {
	id, err := k.rootGroup(ctx)
	if err != nil {
		return nil, err
	}
	var members []struct {
		Username string `json:"username"`
		Enabled  bool   `json:"enabled"`
	}
	if _, err := k.admin(ctx, http.MethodGet, "/groups/"+id+"/members?max=1000", nil, &members); err != nil {
		return nil, err
	}
	var out []string
	for _, m := range members {
		if m.Enabled && !strings.EqualFold(m.Username, k.User) {
			out = append(out, m.Username)
		}
	}
	return out, nil
}

// AddRootAdmin makes the person username, with email and password, and
// puts them in root-admins. A username that is taken is refused.
func (k *Keycloak) AddRootAdmin(ctx context.Context, username, email, password string) error {
	return k.AddPerson(ctx, username, email, password, true)
}

// AddPerson makes the person username, with email and password, a root
// admin if root. A username or email that is taken is ErrInvalid.
func (k *Keycloak) AddPerson(ctx context.Context, username, email, password string, root bool) error {
	gid := ""
	if root {
		var err error
		if gid, err = k.rootGroup(ctx); err != nil {
			return err
		}
	}
	status, err := k.admin(ctx, http.MethodPost, "/users", map[string]any{
		"username": username, "email": email, "emailVerified": true, "enabled": true,
		"firstName": username, "lastName": "-",
		"credentials": []map[string]any{{"type": "password", "value": password, "temporary": false}},
	}, nil)
	if status == http.StatusConflict {
		return fmt.Errorf("%w: the username %s, or that email, is taken", ErrInvalid, username)
	}
	if err != nil {
		return err
	}
	if !root {
		return nil
	}
	u, err := k.user(ctx, username)
	if err != nil {
		return err
	}
	if u == nil {
		return fmt.Errorf("keycloak: %s was made but cannot be found", username)
	}
	_, err = k.admin(ctx, http.MethodPut, "/users/"+u.ID+"/groups/"+gid, nil, nil)
	return err
}

// SetPassword sets the password of the person username, as a reset link
// does; a username that is no person, or the platform's account, is
// ErrInvalid.
func (k *Keycloak) SetPassword(ctx context.Context, username, password string) error {
	if strings.EqualFold(username, k.User) || strings.HasPrefix(username, "agent-") {
		return fmt.Errorf("%w: %s is no person", ErrInvalid, username)
	}
	u, err := k.user(ctx, username)
	if err != nil {
		return err
	}
	if u == nil {
		return fmt.Errorf("%w: there is nobody called %s", ErrInvalid, username)
	}
	_, err = k.admin(ctx, http.MethodPut, "/users/"+u.ID+"/reset-password", map[string]any{"type": "password", "value": password, "temporary": false}, nil)
	return err
}

// RealmPerson is someone who signs in to Garrison.
type RealmPerson struct {
	Username, Email string
	Root            bool // a root admin
	Enabled         bool
}

// People are the people of the realm, sorted, without the platform's
// account and the agents' accounts.
func (k *Keycloak) People(ctx context.Context) ([]RealmPerson, error) {
	var users []struct {
		Username string `json:"username"`
		Email    string `json:"email"`
		Enabled  bool   `json:"enabled"`
	}
	if _, err := k.admin(ctx, http.MethodGet, "/users?briefRepresentation=true&max=2000", nil, &users); err != nil {
		return nil, err
	}
	roots, err := k.RootAdmins(ctx)
	if err != nil {
		return nil, err
	}
	var out []RealmPerson
	for _, u := range users {
		if strings.EqualFold(u.Username, k.User) || strings.HasPrefix(u.Username, "agent-") {
			continue
		}
		out = append(out, RealmPerson{Username: u.Username, Email: u.Email, Enabled: u.Enabled, Root: slices.Contains(roots, u.Username)})
	}
	slices.SortFunc(out, func(a, b RealmPerson) int { return strings.Compare(a.Username, b.Username) })
	return out, nil
}
