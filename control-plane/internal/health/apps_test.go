package health

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"reflect"
	"testing"
)

func TestApps(t *testing.T) {
	run := func(_ context.Context, _ []string, _ []byte, name string, args ...string) ([]byte, error) {
		switch args[1] {
		case "ingresses":
			return []byte(`{"items":[
 {"kind":"Ingress","metadata":{"name":"web","namespace":"app-todo","labels":{"garrison.unit":"todo","garrison.app":"todo","garrison.environment":"main","garrison.io/stack":"todo"}},
  "spec":{"rules":[{"host":"todo.example.com"}]}},
 {"kind":"Ingress","metadata":{"name":"web","namespace":"app-dev-todo","labels":{"garrison.unit":"todo","garrison.app":"todo","garrison.environment":"dev","garrison.io/stack":"todo"}},
  "spec":{"rules":[{"host":"dev-todo.example.com"}]}},
 {"kind":"Ingress","metadata":{"name":"grafana","namespace":"garrison","labels":{"garrison.io/stack":"grafana"}},
  "spec":{"rules":[{"host":"grafana.example.com"}]}},
 {"kind":"Ingress","metadata":{"name":"sso","namespace":"garrison","labels":{"garrison.io/stack":"sso"},"annotations":{"garrison.io/unlisted":"true"}},
  "spec":{"rules":[{"host":"sso.example.com"}]}},
 {"kind":"Ingress","metadata":{"name":"traefik","namespace":"garrison","labels":{"garrison.io/stack":"traefik"}},"spec":{}},
 {"kind":"Ingress","metadata":{"name":"other","namespace":"default"},"spec":{"rules":[{"host":"x.example.com"}]}}
]}`), nil
		case "pods":
			return []byte(`{"items":[
 {"metadata":{"name":"web-1","namespace":"app-todo","labels":{"app":"web"}},"status":{"phase":"Running","containerStatuses":[{"ready":true}]}},
 {"metadata":{"name":"web-2","namespace":"app-dev-todo","labels":{"app":"web"}},"status":{"phase":"Pending"}},
 {"metadata":{"name":"grafana-1","namespace":"garrison","labels":{"app":"grafana"}},"status":{"phase":"Running","containerStatuses":[{"ready":true}]}}
]}`), nil
		}
		return []byte(`{"items":[
 {"kind":"Deployment","metadata":{"name":"web","namespace":"app-todo","labels":{"garrison.unit":"todo","garrison.app":"todo","garrison.environment":"main","garrison.io/stack":"todo"}},
  "spec":{"replicas":1,"selector":{"matchLabels":{"app":"web"}}}},
 {"kind":"Deployment","metadata":{"name":"web","namespace":"app-dev-todo","labels":{"garrison.unit":"todo","garrison.app":"todo","garrison.environment":"dev","garrison.io/stack":"todo"}},
  "spec":{"replicas":1,"selector":{"matchLabels":{"app":"web"}}}},
 {"kind":"Deployment","metadata":{"name":"grafana","namespace":"garrison","labels":{"garrison.io/stack":"grafana"}},
  "spec":{"replicas":1,"selector":{"matchLabels":{"app":"grafana"}}}}
]}`), nil
	}
	k := &Kubernetes{Run: run}
	w := httptest.NewRecorder()
	k.AppsHandler().ServeHTTP(w, httptest.NewRequest("GET", "/apps", nil))
	var got []App
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("%v: %s", err, w.Body)
	}
	want := []App{
		{Unit: "garrison", App: "grafana", Environment: "main", Stack: "grafana", Hosts: []string{"grafana.example.com"}, Desired: 1, Running: 1},
		{Unit: "todo", App: "todo", Environment: "dev", Stack: "todo", Hosts: []string{"dev-todo.example.com"}, Desired: 1, Running: 0},
		{Unit: "todo", App: "todo", Environment: "main", Stack: "todo", Hosts: []string{"todo.example.com"}, Desired: 1, Running: 1},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("apps:\n got %+v\nwant %+v", got, want)
	}
}
