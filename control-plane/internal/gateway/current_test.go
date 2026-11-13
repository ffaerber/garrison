package gateway

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestProposalsAreKeptCurrent(t *testing.T) {
	var updated, comments []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/api/v1")
		switch {
		case p == "/orgs/homepage/repos":
			io.WriteString(w, `[{"name":"todo"},{"name":"config"}]`)
		case p == "/repos/homepage/todo/pulls":
			io.WriteString(w, `[
 {"number":1,"merge_base":"a","mergeable":true,"user":{"login":"bob"},"base":{"ref":"main","sha":"b"},"head":{"ref":"change/bob-1","sha":"h1"}},
 {"number":2,"merge_base":"a","mergeable":false,"user":{"login":"agent-homepage-dev"},"base":{"ref":"main","sha":"b"},"head":{"ref":"agent/dev/carol-2","sha":"h2"}},
 {"number":3,"merge_base":"b","mergeable":true,"user":{"login":"bob"},"base":{"ref":"main","sha":"b"},"head":{"ref":"change/bob-3","sha":"h3"}},
 {"number":4,"merge_base":"a","mergeable":true,"user":{"login":"bob"},"base":{"ref":"main","sha":"b"},"head":{"ref":"feature","sha":"h4"}}]`)
		case strings.HasSuffix(p, "/update"):
			updated = append(updated, r.Header.Get("Sudo")+" "+p)
			if strings.Contains(p, "/2/") {
				w.WriteHeader(http.StatusConflict)
				io.WriteString(w, `{"message":"merge conflict"}`)
				return
			}
			io.WriteString(w, "{}")
		case strings.HasSuffix(p, "/comments"):
			comments = append(comments, p)
			io.WriteString(w, "{}")
		case p == "/repos/homepage/config/pulls":
			t.Error("config's changes were touched")
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	m := &Members{Gitea: &Gitea{URL: srv.URL, User: "admin", Password: "pw"}}
	for range 2 { // the second time, nothing new happens
		if err := m.keepCurrent(context.Background(), "homepage"); err != nil {
			t.Fatal(err)
		}
	}
	if strings.Join(updated, ",") != "bob /repos/homepage/todo/pulls/1/update,agent-homepage-dev /repos/homepage/todo/pulls/2/update" {
		t.Errorf("updated: %v", updated)
	}
	if strings.Join(comments, ",") != "/repos/homepage/todo/issues/2/comments" {
		t.Errorf("comments: %v", comments)
	}
}
