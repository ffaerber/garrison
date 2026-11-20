// Package dashboards keeps Grafana's dashboards as they are in Git. Any repo
// of a unit can hold dashboards as dashboards/*.json; they go into a
// Grafana folder named after the unit, tagged "git". A commit adds,
// changes or removes a dashboard; edits made in Grafana are overwritten.
package dashboards

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Tag marks the dashboards this package owns.
const Tag = "git"

type Grafana struct {
	URL      string // e.g. http://grafana_grafana:3000
	User     string // Grafana's local admin
	Password string
	HTTP     *http.Client

	posted map[string]sent // dashboard uid -> what was last sent
}

// sent is what the sync last wrote to a dashboard: a hash of the body and
// the version Grafana gave it. Any other version means someone edited it in
// Grafana, and it is put back.
type sent struct {
	hash    string
	version int
}

var (
	uidChars = regexp.MustCompile(`[^a-z0-9-]+`)
	validUID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,40}$`)
)

// FolderUID is the uid of a unit's folder.
func FolderUID(unit string) string { return clip("ns-" + unit) }

func clip(s string) string {
	if len(s) > 40 {
		s = s[:40]
	}
	return s
}

func derivedUID(repo, file string) string {
	base := strings.TrimSuffix(filepath.Base(file), ".json")
	return clip(strings.Trim(uidChars.ReplaceAllString(strings.ToLower(repo+"-"+base), "-"), "-"))
}

func (g *Grafana) call(ctx context.Context, method, path string, body, out any) (int, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, g.URL+path, rd)
	if err != nil {
		return 0, err
	}
	req.SetBasicAuth(g.User, g.Password)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	client := g.HTTP
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if resp.StatusCode >= 300 {
		return resp.StatusCode, fmt.Errorf("grafana %s %s: %s: %.200s", method, path, resp.Status, data)
	}
	if out != nil {
		return resp.StatusCode, json.Unmarshal(data, out)
	}
	return resp.StatusCode, nil
}

type dashboard struct {
	uid, repo, file string
	model           map[string]any
}

// read collects the dashboards of a unit's repos, by uid. A file that is
// not a dashboard, or whose uid another file already has, is reported and
// skipped; the others still sync.
func read(repos map[string]string) (map[string]dashboard, []error) {
	out := map[string]dashboard{}
	var errs []error
	names := make([]string, 0, len(repos))
	for n := range repos {
		names = append(names, n)
	}
	slices.Sort(names)
	for _, repo := range names {
		files, _ := filepath.Glob(filepath.Join(repos[repo], "dashboards", "*.json"))
		slices.Sort(files)
		for _, f := range files {
			data, err := os.ReadFile(f)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			var model map[string]any
			if err := json.Unmarshal(data, &model); err != nil {
				errs = append(errs, fmt.Errorf("%s/%s: not JSON: %w", repo, filepath.Base(f), err))
				continue
			}
			if _, ok := model["panels"]; !ok {
				errs = append(errs, fmt.Errorf("%s/%s: not a Grafana dashboard (no panels)", repo, filepath.Base(f)))
				continue
			}
			uid, _ := model["uid"].(string)
			if uid == "" {
				uid = derivedUID(repo, f)
			}
			if !validUID.MatchString(uid) {
				errs = append(errs, fmt.Errorf("%s/%s: invalid uid %q", repo, filepath.Base(f), uid))
				continue
			}
			if other, dup := out[uid]; dup {
				errs = append(errs, fmt.Errorf("%s/%s: uid %q is already used by %s/%s", repo, filepath.Base(f), uid, other.repo, other.file))
				continue
			}
			out[uid] = dashboard{uid: uid, repo: repo, file: filepath.Base(f), model: model}
		}
	}
	return out, errs
}

// Sync makes the unit's Grafana folder hold exactly the dashboards in
// repos, which maps repo names to their checkouts.
func (g *Grafana) Sync(ctx context.Context, unit string, repos map[string]string) error {
	if g.posted == nil {
		g.posted = map[string]sent{}
	}
	folder := FolderUID(unit)
	if status, err := g.call(ctx, http.MethodGet, "/api/folders/"+folder, nil, nil); status == http.StatusNotFound {
		if _, err := g.call(ctx, http.MethodPost, "/api/folders", map[string]string{"uid": folder, "title": unit}, nil); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}

	want, errs := read(repos)
	for _, e := range errs {
		log.Printf("dashboards: %v", e)
	}
	for uid, d := range want {
		m := d.model
		m["uid"] = uid
		m["id"] = nil
		tags, _ := m["tags"].([]any)
		if !slices.Contains(tags, any(Tag)) {
			tags = append(tags, Tag)
		}
		m["tags"] = tags
		body := map[string]any{"dashboard": m, "folderUid": folder, "overwrite": true,
			"message": fmt.Sprintf("From %s/%s/dashboards/%s", unit, d.repo, d.file)}
		raw, _ := json.Marshal(body)
		sum := sha256.Sum256(raw)
		hash := hex.EncodeToString(sum[:])
		if last, ok := g.posted[uid]; ok && last.hash == hash {
			var cur struct {
				Meta struct {
					Version int `json:"version"`
				} `json:"meta"`
			}
			if _, err := g.call(ctx, http.MethodGet, "/api/dashboards/uid/"+url.PathEscape(uid), nil, &cur); err == nil && cur.Meta.Version == last.version {
				continue
			}
		}
		var saved struct {
			Version int `json:"version"`
		}
		if _, err := g.call(ctx, http.MethodPost, "/api/dashboards/db", body, &saved); err != nil {
			log.Printf("dashboards: %s/%s: %v", d.repo, d.file, err)
			continue
		}
		g.posted[uid] = sent{hash: hash, version: saved.Version}
		log.Printf("dashboards: synced %s from %s/%s", uid, d.repo, d.file)
	}

	var found []struct {
		UID string `json:"uid"`
	}
	q := url.Values{"type": {"dash-db"}, "folderUIDs": {folder}, "tag": {Tag}, "limit": {"5000"}}
	if _, err := g.call(ctx, http.MethodGet, "/api/search?"+q.Encode(), nil, &found); err != nil {
		return err
	}
	for _, f := range found {
		if _, ok := want[f.UID]; ok {
			continue
		}
		if _, err := g.call(ctx, http.MethodDelete, "/api/dashboards/uid/"+url.PathEscape(f.UID), nil, nil); err != nil {
			return err
		}
		delete(g.posted, f.UID)
		log.Printf("dashboards: removed %s, no longer in Git", f.UID)
	}
	return nil
}
