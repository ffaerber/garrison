// Package hooks wakes the deployer's loops when something changes in Gitea.
// Each process puts a webhook pointing at itself on every organisation,
// signed with a secret derived from the admin password and never stored; a delivery
// whose signature matches wakes the loops at once instead of at their next
// tick. The ticks stay, for deliveries Gitea fails to make.
//
// Organisation webhooks, because Gitea's API cannot make a system webhook:
// POST /admin/hooks makes a default webhook, which Gitea copies into every
// repo created afterwards, and those copies keep the secret of the day.
package hooks

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Wake fans one signal out to several loops. A loop's pending wake-up is
// kept until it reads it, and several signals in the meantime count as one.
type Wake struct {
	mu   sync.Mutex
	subs []chan struct{}
}

// Subscribe returns a channel that receives after each Poke.
func (w *Wake) Subscribe() <-chan struct{} {
	w.mu.Lock()
	defer w.mu.Unlock()
	c := make(chan struct{}, 1)
	w.subs = append(w.subs, c)
	return c
}

// Poke wakes every subscriber without waiting for it.
func (w *Wake) Poke() {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, c := range w.subs {
		select {
		case c <- struct{}{}:
		default:
		}
	}
}

// Sleep waits for interval, a wake-up on c (which may be nil), or ctx's end.
// It returns false when ctx ended.
func Sleep(ctx context.Context, interval time.Duration, c <-chan struct{}) bool {
	t := time.NewTimer(interval)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
	case <-c:
	}
	return true
}

// Handler accepts Gitea's deliveries signed with Secret and pokes Wake.
type Handler struct {
	Secret []byte
	Wake   *Wake
	// OnEvent, if set, gets each signed delivery: its X-Gitea-Event, its
	// X-Gitea-Delivery and its body. It must not block for long.
	OnEvent func(event, delivery string, body []byte)
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 25<<20))
	if err != nil {
		http.Error(w, "unreadable body", http.StatusBadRequest)
		return
	}
	if !Valid(h.Secret, body, r.Header.Get("X-Gitea-Signature")) {
		http.Error(w, "bad signature", http.StatusUnauthorized)
		return
	}
	h.Wake.Poke()
	if h.OnEvent != nil {
		h.OnEvent(r.Header.Get("X-Gitea-Event"), r.Header.Get("X-Gitea-Delivery"), body)
	}
	w.WriteHeader(http.StatusNoContent)
}

// Valid tells whether signature is the hex HMAC-SHA256 of body with secret.
func Valid(secret, body []byte, signature string) bool {
	if len(secret) == 0 {
		return false
	}
	got, err := hex.DecodeString(signature)
	if err != nil {
		return false
	}
	m := hmac.New(sha256.New, secret)
	m.Write(body)
	return hmac.Equal(got, m.Sum(nil))
}

// Secret derives the webhook secret for target from the admin password, so
// every instance of a process signs alike: after a reboot Docker may run the
// old and the new task side by side, and whichever registers last wins.
func Secret(password, target string) []byte {
	m := hmac.New(sha256.New, []byte(password))
	m.Write([]byte("homecloud webhook " + target))
	return []byte(hex.EncodeToString(m.Sum(nil)))
}

// Events are the Gitea events that may change what the loops do: pushes and
// branches, repos coming and going, pull requests and their reviews, commit
// statuses, and finished pipeline runs.
var Events = []string{
	"create", "delete", "push", "repository",
	"pull_request", "pull_request_sync", "pull_request_review_approved", "pull_request_review_rejected",
	"status", "workflow_run",
}

// Gitea registers the webhooks with Gitea's API, as an admin.
type Gitea struct {
	URL, User, Password string
	Client              *http.Client
}

type hook struct {
	ID     int64             `json:"id"`
	Config map[string]string `json:"config"`
}

// Register points a webhook of organisation org at target, signed with
// secret, creating it or updating the one already pointing there.
func (g *Gitea) Register(ctx context.Context, org, target string, secret []byte) error {
	var hooks []hook
	base := "/api/v1/orgs/" + url.PathEscape(org) + "/hooks"
	if err := g.do(ctx, http.MethodGet, base+"?limit=50", nil, &hooks); err != nil {
		return err
	}
	body := map[string]any{
		"type":   "gitea",
		"active": true,
		"events": Events,
		"config": map[string]string{"url": target, "content_type": "json", "secret": string(secret)},
	}
	for _, h := range hooks {
		if h.Config["url"] == target {
			return g.do(ctx, http.MethodPatch, fmt.Sprintf("%s/%d", base, h.ID), body, nil)
		}
	}
	return g.do(ctx, http.MethodPost, base, body, nil)
}

// Keep registers the webhook on every organisation, now and on each new one,
// checking every interval until ctx ends. Gitea may still be starting.
func (g *Gitea) Keep(ctx context.Context, target string, secret []byte, interval time.Duration) {
	done := map[string]bool{}
	for {
		type org struct {
			Name string `json:"username"`
		}
		// Every organisation, page by page: Gitea lists 50 at once.
		var orgs []org
		for page := 1; page <= 100; page++ {
			var batch []org
			if err := g.do(ctx, http.MethodGet, fmt.Sprintf("/api/v1/admin/orgs?limit=50&page=%d", page), nil, &batch); err != nil {
				log.Printf("webhook: %v", err)
				break
			}
			orgs = append(orgs, batch...)
			if len(batch) < 50 {
				break
			}
		}
		for _, o := range orgs {
			if done[o.Name] {
				continue
			}
			if err := g.Register(ctx, o.Name, target, secret); err != nil {
				log.Printf("webhook on %s: %v", o.Name, err)
				continue
			}
			done[o.Name] = true
			log.Printf("Gitea notifies %s of %s", target, o.Name)
		}
		if !Sleep(ctx, interval, nil) {
			return
		}
	}
}

func (g *Gitea) do(ctx context.Context, method, path string, in, out any) error {
	var r io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(g.URL, "/")+path, r)
	if err != nil {
		return err
	}
	req.SetBasicAuth(g.User, g.Password)
	req.Header.Set("Content-Type", "application/json")
	c := g.Client
	if c == nil {
		c = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("%s %s: %s: %s", method, path, resp.Status, strings.TrimSpace(string(data)))
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}
