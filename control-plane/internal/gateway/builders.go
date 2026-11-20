package gateway

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"garrison/control-plane/internal/hooks"
)

// Builders gives every unit other than garrison the credentials its
// pipelines push images with: a build account, <unit>-builder, in the
// organisation's team "builders", which may write packages and nothing else,
// with a token limited to packages. The token and the registry's address
// become the organisation's Actions secrets REGISTRY_USER and REGISTRY_TOKEN
// and variable REGISTRY. Gitea's own job tokens cannot push to an
// organisation's registry, and a unit's account can push only there.
type Builders struct {
	// Wake, when set, starts a sync before the interval is over.
	Wake   <-chan struct{}
	Gitea  *Gitea
	Domain string
	// CA is the installation's certificate, when it is self-signed: published
	// as the variable REGISTRY_CA, so pipelines can trust the registry.
	CA string
}

// BuilderName is the build account of a unit.
func BuilderName(unit string) string { return unit + "-builder" }

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// Sync sets up every unit that lacks its build credentials.
func (b *Builders) Sync(ctx context.Context) error {
	units, err := b.Gitea.Units(ctx, "")
	if err != nil {
		return err
	}
	present := map[string]bool{}
	for _, ns := range units {
		present[ns.Name] = true
		if ns.Name == PlatformUnit {
			continue
		}
		if err := b.setup(ctx, ns.Name); err != nil {
			return fmt.Errorf("build account of %s: %w", ns.Name, err)
		}
	}
	return b.removeGone(ctx, present)
}

// removeGone deletes the build accounts of units that are gone, and
// with them their tokens: a unit made again under the same name gets
// an account of its own, and the old token pushes nowhere.
func (b *Builders) removeGone(ctx context.Context, present map[string]bool) error {
	users, err := b.Gitea.users(ctx)
	if err != nil {
		return err
	}
	for _, u := range users {
		ns, ok := strings.CutSuffix(u.Login, "-builder")
		if !ok || present[ns] || u.Email != u.Login+"@noreply."+b.Domain {
			continue
		}
		if _, err := b.Gitea.do(ctx, "", http.MethodDelete, "/admin/users/"+u.Login, nil, nil); err != nil {
			return fmt.Errorf("remove the build account of %s, which is gone: %w", ns, err)
		}
		log.Printf("builders: removed %s: its unit is gone", u.Login)
	}
	return nil
}

func (b *Builders) setup(ctx context.Context, ns string) error {
	g := b.Gitea
	// The registry's address and certificate, kept current.
	vars := map[string]string{"REGISTRY": "git." + b.Domain}
	if b.CA != "" {
		vars["REGISTRY_CA"] = b.CA
	}
	for name, value := range vars {
		body := map[string]string{"value": value}
		if _, err := g.do(ctx, "", http.MethodPut, "/orgs/"+ns+"/actions/variables/"+name, body, nil); err != nil {
			if _, err := g.do(ctx, "", http.MethodPost, "/orgs/"+ns+"/actions/variables/"+name, body, nil); err != nil {
				return err
			}
		}
	}
	var secrets []struct {
		Name string `json:"name"`
	}
	if _, err := g.do(ctx, "", http.MethodGet, "/orgs/"+ns+"/actions/secrets", nil, &secrets); err != nil {
		return err
	}
	for _, s := range secrets {
		if s.Name == "REGISTRY_TOKEN" {
			return nil
		}
	}

	// The team that may write packages, and nothing else.
	var teams []struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
	}
	if _, err := g.do(ctx, "", http.MethodGet, "/orgs/"+ns+"/teams", nil, &teams); err != nil {
		return err
	}
	var team int64
	for _, t := range teams {
		if t.Name == "builders" {
			team = t.ID
		}
	}
	if team == 0 {
		var created struct {
			ID int64 `json:"id"`
		}
		if _, err := g.do(ctx, "", http.MethodPost, "/orgs/"+ns+"/teams", map[string]any{
			"name": "builders", "description": "Build accounts of this unit's pipelines: may push images.",
			"permission": "read", "units_map": map[string]string{"repo.packages": "write"},
			"includes_all_repositories": false, "can_create_org_repo": false,
		}, &created); err != nil {
			return err
		}
		team = created.ID
	}

	// The account, with a fresh password nobody keeps.
	user, password := BuilderName(ns), randomHex(24)
	account := map[string]any{"login_name": user, "source_id": 0, "password": password, "must_change_password": false}
	if status, err := g.do(ctx, "", http.MethodGet, "/users/"+user, nil, nil); status == http.StatusNotFound {
		account["username"] = user
		account["email"] = user + "@noreply." + b.Domain
		account["visibility"] = "private"
		if _, err := g.do(ctx, "", http.MethodPost, "/admin/users", account, nil); err != nil {
			return err
		}
	} else if err != nil {
		return err
	} else if _, err := g.do(ctx, "", http.MethodPatch, "/admin/users/"+user, account, nil); err != nil {
		return err
	}
	if _, err := g.do(ctx, "", http.MethodPut, fmt.Sprintf("/teams/%d/members/%s", team, user), nil, nil); err != nil {
		return err
	}
	token, err := b.newToken(ctx, user, password)
	if err != nil {
		return err
	}
	for name, value := range map[string]string{"REGISTRY_USER": user, "REGISTRY_TOKEN": token} {
		if _, err := g.do(ctx, "", http.MethodPut, "/orgs/"+ns+"/actions/secrets/"+name, map[string]string{"data": value}, nil); err != nil {
			return err
		}
	}
	log.Printf("builders: unit %s can push images as %s", ns, user)
	return nil
}

// newToken creates a token limited to packages, as the build account itself:
// only an account may create its own tokens.
func (b *Builders) newToken(ctx context.Context, user, password string) (string, error) {
	body, _ := json.Marshal(map[string]any{"name": fmt.Sprintf("registry-%d", time.Now().Unix()), "scopes": []string{"write:package"}})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.Gitea.URL+"/api/v1/users/"+user+"/tokens", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.SetBasicAuth(user, password)
	req.Header.Set("Content-Type", "application/json")
	client := b.Gitea.HTTP
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("create token: %s: %.200s", resp.Status, data)
	}
	var out struct {
		SHA1 string `json:"sha1"`
	}
	if err := json.Unmarshal(data, &out); err != nil || out.SHA1 == "" {
		return "", fmt.Errorf("create token: no token in answer")
	}
	return out.SHA1, nil
}

// Run syncs now and then every interval, logging failures.
func (b *Builders) Run(ctx context.Context, interval time.Duration) {
	for {
		if err := b.Sync(ctx); err != nil {
			log.Printf("builders: %v", err)
		}
		if !hooks.Sleep(ctx, interval, b.Wake) {
			return
		}
	}
}
