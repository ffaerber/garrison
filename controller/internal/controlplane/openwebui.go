package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

// OpenWebUI keeps one chat agent per namespace in Open WebUI: a model preset
// named after the namespace, on the configured LLM, with this control plane's
// MCP tools and a prompt for that namespace. It signs in as the local admin
// Open WebUI creates from its environment at first start.
type OpenWebUI struct {
	URL          string // e.g. http://openwebui_openwebui:8080
	Email        string
	Password     string
	BaseModel    string // e.g. gpt-oss:120b
	ToolServerID string // the MCP connection's id in Open WebUI
	Domain       string
	Gitea        *Gitea
	HTTP         *http.Client
}

// ModelPrefix marks the models this sync owns.
const ModelPrefix = "ns-"

type owuiModel struct {
	ID           string         `json:"id"`
	BaseModelID  string         `json:"base_model_id"`
	Name         string         `json:"name"`
	Meta         map[string]any `json:"meta"`
	Params       map[string]any `json:"params"`
	AccessGrants []any          `json:"access_grants"`
	IsActive     bool           `json:"is_active"`
}

func (o *OpenWebUI) prompt(ns Namespace) string {
	return fmt.Sprintf(`You are the operator agent of the namespace %[1]q of a home cloud at %[2]s. You answer questions about its stacks and propose changes to it, using your tools. Pass namespace %[1]q to every tool that takes one.

How it works:
- A namespace is a Gitea organisation. Each of its repos that holds a compose.yaml is one Docker Swarm stack, named after the repo. The controller deploys the main branch of each such repo.
- Hostnames are <name>.%[2]s. People sign in through Keycloak.
- Logs: service_logs. Metrics: service_usage for CPU and memory of a stack, query_metrics for any PromQL.

Rules:
- Look things up with the tools; do not guess. Say what you found.
- You act with the rights of the person you talk to. If a tool says something is not allowed, say so plainly; do not work around it.
- To change anything, read the current file, then call propose_change with the whole new file. That opens a pull request; nothing changes until a person allowed to merges it in Gitea. Always give them the link, and never say a change is made or deployed when you only proposed it.
- You cannot merge, deploy or read secret values.
- Be brief.`, ns.Name, o.Domain)
}

func (o *OpenWebUI) want(ns Namespace) owuiModel {
	desc := "Operator agent of the namespace " + ns.Name
	if ns.Description != "" {
		desc += ": " + ns.Description
	}
	return owuiModel{
		ID:          ModelPrefix + ns.Name,
		BaseModelID: o.BaseModel,
		Name:        ns.Name,
		Meta: map[string]any{
			"description": desc,
			"toolIds":     []string{"server:mcp:" + o.ToolServerID},
		},
		Params: map[string]any{
			"system":           o.prompt(ns),
			"function_calling": "native",
		},
		// Everyone signed in may chat with every namespace's agent; the tools
		// still act with each person's own rights.
		AccessGrants: []any{map[string]any{"principal_type": "user", "principal_id": "*", "permission": "read"}},
		IsActive:     true,
	}
}

func (o *OpenWebUI) client() *http.Client {
	if o.HTTP != nil {
		return o.HTTP
	}
	return &http.Client{Timeout: 30 * time.Second}
}

func (o *OpenWebUI) call(ctx context.Context, token, method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, o.URL+path, rd)
	if err != nil {
		return err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := o.client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("open webui %s %s: %s: %.200s", method, path, resp.Status, data)
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

// Sync makes Open WebUI's ns-* models match the namespaces in Gitea.
func (o *OpenWebUI) Sync(ctx context.Context) error {
	var signin struct {
		Token string `json:"token"`
	}
	if err := o.call(ctx, "", http.MethodPost, "/api/v1/auths/signin", map[string]string{"email": o.Email, "password": o.Password}, &signin); err != nil {
		return err
	}
	namespaces, err := o.Gitea.Namespaces(ctx, "")
	if err != nil {
		return err
	}
	var all []owuiModel
	if err := o.call(ctx, signin.Token, http.MethodGet, "/api/v1/models/all", nil, &all); err != nil {
		return err
	}
	have := map[string]owuiModel{}
	base := false
	for _, m := range all {
		if strings.HasPrefix(m.ID, ModelPrefix) {
			have[m.ID] = m
		}
		base = base || m.ID == o.BaseModel
	}
	// Open WebUI lets only admins use a base model that has no entry of its
	// own, also through a preset. The entry makes it readable to everyone
	// signed in, and hides it from the model list: people chat with the
	// namespace agents. An entry an admin made is left as it is.
	if !base {
		entry := owuiModel{
			ID: o.BaseModel, Name: o.BaseModel,
			Meta:         map[string]any{"description": "The LLM behind the namespace agents.", "hidden": true},
			Params:       map[string]any{},
			AccessGrants: []any{map[string]any{"principal_type": "user", "principal_id": "*", "permission": "read"}},
			IsActive:     true,
		}
		if err := o.call(ctx, signin.Token, http.MethodPost, "/api/v1/models/create", entry, nil); err != nil {
			return err
		}
		log.Printf("open webui: made the base model %s usable for everyone signed in", o.BaseModel)
	}
	for _, ns := range namespaces {
		want := o.want(ns)
		cur, ok := have[want.ID]
		delete(have, want.ID)
		switch {
		case !ok:
			if err := o.call(ctx, signin.Token, http.MethodPost, "/api/v1/models/create", want, nil); err != nil {
				return err
			}
			log.Printf("open webui: added the agent of namespace %s", ns.Name)
		case !same(cur, want):
			if err := o.call(ctx, signin.Token, http.MethodPost, "/api/v1/models/model/update", want, nil); err != nil {
				return err
			}
			log.Printf("open webui: updated the agent of namespace %s", ns.Name)
		}
	}
	for id := range have {
		if err := o.call(ctx, signin.Token, http.MethodPost, "/api/v1/models/model/delete", map[string]string{"id": id}, nil); err != nil {
			return err
		}
		log.Printf("open webui: removed %s, whose namespace is gone", id)
	}
	return nil
}

// same compares the fields the sync owns. Access grants are set on every
// create and update but not compared: Open WebUI returns them in its own shape.
func same(a, b owuiModel) bool {
	if a.Name != b.Name || a.BaseModelID != b.BaseModelID || !a.IsActive {
		return false
	}
	ja, _ := json.Marshal([]any{a.Params["system"], a.Params["function_calling"], a.Meta["toolIds"], a.Meta["description"]})
	jb, _ := json.Marshal([]any{b.Params["system"], b.Params["function_calling"], b.Meta["toolIds"], b.Meta["description"]})
	return bytes.Equal(ja, jb)
}

// Run syncs now and then every interval, logging failures; Open WebUI may
// still be starting.
func (o *OpenWebUI) Run(ctx context.Context, interval time.Duration) {
	for {
		if err := o.Sync(ctx); err != nil {
			log.Printf("open webui sync: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}
