package health

import (
	"context"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestMetrics(t *testing.T) {
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	run := func(_ context.Context, _ []string, _ []byte, name string, args ...string) ([]byte, error) {
		if args[1] == "ingresses" {
			return []byte(`{"items":[
 {"kind":"Ingress","metadata":{"name":"web","namespace":"app-todo","labels":{"garrison.unit":"todo","garrison.app":"todo","garrison.environment":"main","garrison.io/stack":"todo"}},
  "spec":{"rules":[{"host":"todo.example.com","http":{"paths":[{"backend":{"service":{"name":"web","port":{"name":"http"}}}}]}}]}},
 {"kind":"Ingress","metadata":{"name":"gitea","namespace":"garrison","labels":{"garrison.io/stack":"gitea"}},
  "spec":{"rules":[{"http":{"paths":[{"backend":{"service":{"name":"gitea","port":{"name":"http"}}}}]}}]}}
]}`), nil
		}
		if args[1] == "pods" {
			return []byte(`{"items":[
 {"metadata":{"name":"web-1","namespace":"app-quotes","labels":{"app":"web"}},"status":{"phase":"Running","containerStatuses":[
   {"ready":false,"lastState":{"terminated":{"exitCode":1,"finishedAt":"2026-11-14T11:59:40Z"}}}]}},
 {"metadata":{"name":"web-2","namespace":"app-quotes","labels":{"app":"web"}},"status":{"phase":"Pending","containerStatuses":[
   {"ready":false,"state":{"terminated":{"exitCode":2,"finishedAt":"2026-11-14T11:58:00Z"}}}]}},
 {"metadata":{"name":"web-3","namespace":"app-quotes","labels":{"app":"web"}},"status":{"phase":"Running","containerStatuses":[
   {"ready":false,"lastState":{"terminated":{"exitCode":1,"finishedAt":"2026-11-14T11:00:00Z"}}}]}},
 {"metadata":{"name":"gitea-x","namespace":"garrison","labels":{"app":"gitea"}},"status":{"phase":"Running","containerStatuses":[{"ready":true}]}},
 {"metadata":{"name":"web-elsewhere","namespace":"garrison","labels":{"app":"web"}},"status":{"phase":"Running","containerStatuses":[{"ready":true}]}},
 {"metadata":{"name":"garrison-agent-1","namespace":"garrison-sandboxes","labels":{"garrison.sandbox":"true","garrison.agent":"todo_developer"},
   "annotations":{"garrison.unit":"todo","garrison.agent":"todo/developer","garrison.person":"bob","garrison.chat":"channel:abc"}},"status":{"phase":"Running"}}
]}`), nil
		}
		return []byte(`{"items":[
 {"kind":"Deployment","metadata":{"name":"web","namespace":"app-quotes","labels":{"garrison.unit":"todo","garrison.app":"quotes","garrison.environment":"main","garrison.io/stack":"quotes"}},
  "spec":{"replicas":3,"selector":{"matchLabels":{"app":"web"}}}},
 {"kind":"Deployment","metadata":{"name":"gitea","namespace":"garrison","labels":{"garrison.io/stack":"gitea"}},
  "spec":{"replicas":1,"selector":{"matchLabels":{"app":"gitea"}}}},
 {"kind":"Deployment","metadata":{"name":"coredns","namespace":"kube-system","labels":{"k8s-app":"kube-dns"}},
  "spec":{"replicas":1,"selector":{"matchLabels":{"k8s-app":"kube-dns"}}}}
]}`), nil
	}
	k := &Kubernetes{Run: run, Now: func() time.Time { return now }}
	w := httptest.NewRecorder()
	k.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))
	body := w.Body.String()
	for _, want := range []string{
		"garrison_cluster_up 1",
		`garrison_service_replicas_desired{unit="todo",app="quotes",environment="main",stack="quotes",service="quotes_web"} 3`,
		`garrison_service_replicas_running{unit="todo",app="quotes",environment="main",stack="quotes",service="quotes_web"} 0`,
		`garrison_service_failed_tasks{unit="todo",app="quotes",environment="main",stack="quotes",service="quotes_web"} 2`,
		`garrison_service_replicas_running{unit="garrison",app="gitea",environment="main",stack="gitea",service="gitea"} 1`,
		`garrison_pod_info{namespace="garrison",pod="gitea-x",unit="garrison",app="gitea",environment="main",stack="gitea",service="gitea"} 1`,
		`garrison_route_info{service="app-todo-web-http@kubernetes",unit="todo",app="todo",environment="main",stack="todo"} 1`,
		`garrison_sandbox_info{namespace="garrison-sandboxes",pod="garrison-agent-1",unit="todo",agent="todo/developer",person="bob",chat="channel:abc"} 1`,
		`garrison_pod_info{namespace="app-quotes",pod="web-2",unit="todo",app="quotes",environment="main",stack="quotes",service="quotes_web"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %s in\n%s", want, body)
		}
	}
	if strings.Contains(body, "coredns") {
		t.Errorf("Kubernetes's own workloads are counted:\n%s", body)
	}
}

// A container that keeps failing restarts in the same pod, which shows only
// its last exit: each restart after an error counts, while within Window.
func TestRestartsInTheSamePodCount(t *testing.T) {
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	restarts := 1
	run := func(_ context.Context, _ []string, _ []byte, name string, args ...string) ([]byte, error) {
		switch args[1] {
		case "pods":
			return []byte(`{"items":[{"metadata":{"name":"db-1","namespace":"app-crash","labels":{"app":"db"}},"status":{"phase":"Running","containerStatuses":[
 {"name":"db","ready":false,"restartCount":` + fmt.Sprint(restarts) + `,"lastState":{"terminated":{"exitCode":1,"finishedAt":"` + now.Add(-5*time.Second).Format(time.RFC3339) + `"}}}]}}]}`), nil
		case "ingresses":
			return []byte(`{"items":[]}`), nil
		}
		return []byte(`{"items":[{"kind":"Deployment","metadata":{"name":"db","namespace":"app-crash","labels":{"garrison.unit":"todo","garrison.app":"crash","garrison.environment":"main","garrison.io/stack":"crash"}},
  "spec":{"replicas":1,"selector":{"matchLabels":{"app":"db"}}}}]}`), nil
	}
	k := &Kubernetes{Run: run, Now: func() time.Time { return now }}
	failed := func() int {
		svcs, err := k.Services(context.Background())
		if err != nil || len(svcs) != 1 {
			t.Fatal(svcs, err)
		}
		return svcs[0].Failed
	}
	if n := failed(); n != 1 {
		t.Errorf("first look: %d failed, want 1", n)
	}
	now, restarts = now.Add(time.Minute), 3
	if n := failed(); n != 3 {
		t.Errorf("two restarts later: %d failed, want 3", n)
	}
	if n := failed(); n != 3 {
		t.Errorf("looking again counts again: %d", n)
	}
	now = now.Add(Window)
	if n := failed(); n != 0 {
		t.Errorf("after the window: %d failed, want 0", n)
	}
}
