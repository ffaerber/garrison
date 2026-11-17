package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"garrison/control-plane/internal/agents"
)

// An agent whose repo went private must lose its access for everyone: Open
// WebUI's model update keeps the grants, so they are set on their own.
func TestAgentThatTurnsPrivateLosesItsAccess(t *testing.T) {
	a := agents.Agent{Namespace: "crud", Name: "helper", Description: "Helps"}
	o := &OpenWebUI{}
	models := map[string]owuiModel{a.ID(): o.want(a, nil, false)}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/models/all":
			var list []owuiModel
			for _, m := range models {
				list = append(list, m)
			}
			json.NewEncoder(w).Encode(list)
		case "/api/v1/models/model/update":
			var m owuiModel
			json.NewDecoder(r.Body).Decode(&m)
			m.AccessGrants = models[m.ID].AccessGrants // as Open WebUI
			models[m.ID] = m
		case "/api/v1/models/model/access/update":
			var b struct {
				ID           string `json:"id"`
				AccessGrants []any  `json:"access_grants"`
			}
			json.NewDecoder(r.Body).Decode(&b)
			m := models[b.ID]
			m.AccessGrants = b.AccessGrants
			models[b.ID] = m
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	o.URL = srv.URL
	err := o.syncModels(context.Background(), "t", []agents.Agent{a}, map[string]string{"crud": "g1"}, map[string]bool{a.ID(): true})
	if err != nil {
		t.Fatal(err)
	}
	grants, _ := json.Marshal(models[a.ID()].AccessGrants)
	if strings.Contains(string(grants), `"*"`) || !strings.Contains(string(grants), `"g1"`) {
		t.Errorf("access grants = %s, want only the group g1", grants)
	}
}

// In the chat, an agent goes by its name: it is the same agent in every
// channel. The model's ID is garrison/<name>.
func TestAnAgentGoesByItsNameAlone(t *testing.T) {
	o := &OpenWebUI{}
	for _, c := range []struct{ ns, name, id, shown string }{
		{"garrison", "commander", "garrison/commander", "commander"},
		{"garrison", "operator", "garrison/operator", "operator"},
	} {
		m := o.want(agents.Agent{Namespace: c.ns, Name: c.name}, nil, false)
		if m.ID != c.id || m.Name != c.shown {
			t.Errorf("%s/%s: id %q, name %q", c.ns, c.name, m.ID, m.Name)
		}
	}
}
