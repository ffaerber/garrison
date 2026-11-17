package dashboards

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

type fakeGrafana struct {
	mu      sync.Mutex
	folders map[string]string
	boards  map[string]map[string]any // uid -> {dashboard, folderUid}
	version map[string]int
	posts   int
}

func (f *fakeGrafana) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if u, p, _ := r.BasicAuth(); u != "installer" || p != "pw" {
		http.Error(w, "unauthorized", 401)
		return
	}
	switch {
	case strings.HasPrefix(r.URL.Path, "/api/folders/") && r.Method == "GET":
		if _, ok := f.folders[strings.TrimPrefix(r.URL.Path, "/api/folders/")]; !ok {
			http.Error(w, `{"message":"not found"}`, 404)
			return
		}
		io.WriteString(w, "{}")
	case r.URL.Path == "/api/folders":
		var b map[string]string
		json.NewDecoder(r.Body).Decode(&b)
		f.folders[b["uid"]] = b["title"]
		io.WriteString(w, "{}")
	case r.URL.Path == "/api/dashboards/db":
		var b map[string]any
		json.NewDecoder(r.Body).Decode(&b)
		d := b["dashboard"].(map[string]any)
		uid := d["uid"].(string)
		f.boards[uid] = b
		f.version[uid]++
		f.posts++
		json.NewEncoder(w).Encode(map[string]int{"version": f.version[uid]})
	case strings.HasPrefix(r.URL.Path, "/api/dashboards/uid/") && r.Method == "GET":
		uid := strings.TrimPrefix(r.URL.Path, "/api/dashboards/uid/")
		json.NewEncoder(w).Encode(map[string]any{"meta": map[string]int{"version": f.version[uid]}})
	case r.URL.Path == "/api/search":
		folder, tag := r.URL.Query().Get("folderUIDs"), r.URL.Query().Get("tag")
		var out []map[string]string
		for uid, b := range f.boards {
			tags := b["dashboard"].(map[string]any)["tags"].([]any)
			has := false
			for _, t := range tags {
				has = has || t == tag
			}
			if b["folderUid"] == folder && has {
				out = append(out, map[string]string{"uid": uid})
			}
		}
		json.NewEncoder(w).Encode(out)
	case strings.HasPrefix(r.URL.Path, "/api/dashboards/uid/") && r.Method == "DELETE":
		delete(f.boards, strings.TrimPrefix(r.URL.Path, "/api/dashboards/uid/"))
		io.WriteString(w, "{}")
	default:
		http.NotFound(w, r)
	}
}

func write(t *testing.T, dir, name, content string) {
	t.Helper()
	os.MkdirAll(filepath.Join(dir, "dashboards"), 0o755)
	if err := os.WriteFile(filepath.Join(dir, "dashboards", name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestSync(t *testing.T) {
	fg := &fakeGrafana{folders: map[string]string{}, version: map[string]int{}, boards: map[string]map[string]any{
		// Made by hand in Grafana, in the same folder, not from Git: kept.
		"by-hand": {"folderUid": "ns-garrison", "dashboard": map[string]any{"uid": "by-hand", "tags": []any{"mine"}}},
	}}
	srv := httptest.NewServer(fg)
	defer srv.Close()
	g := &Grafana{URL: srv.URL, User: "installer", Password: "pw"}

	grafana, app := t.TempDir(), t.TempDir()
	write(t, grafana, "overview.json", `{"uid":"garrison-overview","title":"Overview","panels":[],"tags":["garrison"]}`)
	write(t, grafana, "noid.json", `{"title":"No uid","panels":[]}`)
	write(t, grafana, "broken.json", `{not json`)
	write(t, app, "dupe.json", `{"uid":"garrison-overview","title":"Dupe","panels":[]}`)
	write(t, app, "notadash.json", `{"title":"no panels"}`)
	repos := map[string]string{"grafana": grafana, "zapp": app, "empty": t.TempDir()}

	if err := g.Sync(context.Background(), "garrison", repos); err != nil {
		t.Fatal(err)
	}
	if fg.folders["ns-garrison"] != "garrison" {
		t.Errorf("folders = %v", fg.folders)
	}
	if len(fg.boards) != 3 {
		t.Fatalf("boards = %v", fg.boards)
	}
	ov := fg.boards["garrison-overview"]["dashboard"].(map[string]any)
	if ov["title"] != "Overview" {
		t.Errorf("the first repo by name should keep a uid, got %v", ov["title"])
	}
	if tags := ov["tags"].([]any); len(tags) != 2 || tags[1] != Tag {
		t.Errorf("tags = %v", tags)
	}
	if _, ok := fg.boards["grafana-noid"]; !ok {
		t.Errorf("a dashboard without uid did not get one derived from its repo and file: %v", fg.boards)
	}

	// Nothing changed: nothing is sent again.
	before := fg.posts
	g.Sync(context.Background(), "garrison", repos)
	if fg.posts != before {
		t.Errorf("unchanged dashboards were sent again")
	}

	// Someone edits a Git dashboard in Grafana: the next sync puts it back.
	fg.boards["garrison-overview"]["dashboard"].(map[string]any)["title"] = "Edited in Grafana"
	fg.version["garrison-overview"]++
	g.Sync(context.Background(), "garrison", repos)
	if fg.boards["garrison-overview"]["dashboard"].(map[string]any)["title"] != "Overview" {
		t.Error("an edit made in Grafana was not put back")
	}

	// A file removed from Git disappears from Grafana; the hand-made one stays.
	os.Remove(filepath.Join(grafana, "dashboards", "noid.json"))
	g.Sync(context.Background(), "garrison", repos)
	if _, ok := fg.boards["grafana-noid"]; ok {
		t.Error("a dashboard removed from Git was kept")
	}
	if _, ok := fg.boards["by-hand"]; !ok {
		t.Error("a dashboard not from Git was removed")
	}

	// A changed file is sent again.
	write(t, grafana, "overview.json", `{"uid":"garrison-overview","title":"Overview 2","panels":[]}`)
	g.Sync(context.Background(), "garrison", repos)
	if fg.boards["garrison-overview"]["dashboard"].(map[string]any)["title"] != "Overview 2" {
		t.Error("a changed dashboard was not updated")
	}
}
