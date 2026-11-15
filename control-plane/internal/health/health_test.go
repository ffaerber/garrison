package health

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestMetrics(t *testing.T) {
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	docker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/services":
			w.Write([]byte(`[
 {"ID":"s1","Spec":{"Name":"quotes_web","Labels":{"com.docker.stack.namespace":"quotes","garrison.namespace":"todo","garrison.app":"quotes","garrison.environment":"main"}}},
 {"ID":"s2","Spec":{"Name":"gitea_gitea","Labels":{"com.docker.stack.namespace":"gitea"}}}
]`))
		case "/tasks":
			w.Write([]byte(`[
 {"ServiceID":"s1","DesiredState":"running","Status":{"State":"starting","Timestamp":"2026-11-14T11:59:50Z"}},
 {"ServiceID":"s1","DesiredState":"shutdown","Status":{"State":"failed","Timestamp":"2026-11-14T11:59:40Z","Err":"task: non-zero exit (1)"}},
 {"ServiceID":"s1","DesiredState":"shutdown","Status":{"State":"failed","Timestamp":"2026-11-14T11:58:00Z"}},
 {"ServiceID":"s1","DesiredState":"shutdown","Status":{"State":"failed","Timestamp":"2026-11-14T11:00:00Z"}},
 {"ServiceID":"s2","DesiredState":"running","Status":{"State":"running","Timestamp":"2026-11-14T08:00:00Z"}}
]`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer docker.Close()
	d := &Docker{HTTP: &http.Client{Transport: rewrite{docker.URL}}, Now: func() time.Time { return now }}
	w := httptest.NewRecorder()
	d.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))
	body := w.Body.String()
	for _, want := range []string{
		"garrison_docker_up 1",
		`garrison_service_replicas_desired{namespace="todo",app="quotes",environment="main",stack="quotes",service="quotes_web"} 1`,
		`garrison_service_replicas_running{namespace="todo",app="quotes",environment="main",stack="quotes",service="quotes_web"} 0`,
		`garrison_service_failed_tasks{namespace="todo",app="quotes",environment="main",stack="quotes",service="quotes_web"} 2`,
		`garrison_service_replicas_running{namespace="system",app="gitea",environment="main",stack="gitea",service="gitea_gitea"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %s in\n%s", want, body)
		}
	}
}

// rewrite sends every request to the test server.
type rewrite struct{ base string }

func (r rewrite) RoundTrip(req *http.Request) (*http.Response, error) {
	u := *req.URL
	u.Scheme, u.Host = "http", strings.TrimPrefix(r.base, "http://")
	req = req.Clone(req.Context())
	req.URL = &u
	return http.DefaultTransport.RoundTrip(req)
}
