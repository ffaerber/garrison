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

// OpenWebUI keeps, for every namespace, an agent and a channel in Open WebUI.
// The agent is a model preset named after the namespace, on the configured
// LLM, with this control plane's MCP tools and a prompt for that namespace.
// The channel is the namespace's team chat: everyone signed in reads and
// posts there, and mentioning the agent makes it answer, with the tools
// acting as the person who mentioned it. Only admins may change a channel,
// and the sync puts back what it owns. It signs in as the local admin Open
// WebUI creates from its environment at first start.
type OpenWebUI struct {
	URL          string // e.g. http://openwebui_openwebui:8080
	Email        string
	Password     string
	BaseModel    string // e.g. gpt-oss:120b
	ToolServerID string // the MCP connection's id in Open WebUI
	Domain       string
	Gitea        *Gitea
	HTTP         *http.Client

	accountID string // the control plane's own Open WebUI account
}

// ModelPrefix marks the models this sync owns: agent-<namespace>, which is
// also the name people mention, e.g. @agent-system.
const ModelPrefix = "agent-"

// legacyPrefix marks agents of earlier versions, which the sync removes.
const legacyPrefix = "ns-"

// AgentName is the id and name of a namespace's agent.
func AgentName(namespace string) string { return ModelPrefix + namespace }

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
	return fmt.Sprintf(`You are %[3]s, the operator agent of the namespace %[1]q of a home cloud at %[2]s. You answer questions about its stacks and propose changes to it, using your tools. Pass namespace %[1]q to every tool that takes one.

How it works:
- A namespace is a Gitea organisation. Each of its repos that holds a compose.yaml is one Docker Swarm stack, named after the repo. The controller deploys the main branch of each such repo.
- Hostnames are <name>.%[2]s. People sign in through Keycloak.
- Logs: service_logs. Metrics: service_usage for CPU and memory of a stack, query_metrics for any PromQL.

Rules:
- Look things up with the tools; do not guess. Say what you found.
- You act with the rights of the person you talk to. If a tool says something is not allowed, say so plainly; do not work around it.
- To change anything, read the current file, then call propose_change with the whole new file. That opens a pull request; nothing changes until a person allowed to merges it in Gitea. Always give them the link, and never say a change is made or deployed when you only proposed it.
- You cannot merge, deploy or read secret values.
- Be brief.`, ns.Name, o.Domain, AgentName(ns.Name))
}

func (o *OpenWebUI) want(ns Namespace) owuiModel {
	desc := "Operator agent of the namespace " + ns.Name
	if ns.Description != "" {
		desc += ": " + ns.Description
	}
	return owuiModel{
		ID:          AgentName(ns.Name),
		BaseModelID: o.BaseModel,
		Name:        AgentName(ns.Name),
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
	if err := o.syncProfile(ctx, signin.Token); err != nil {
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
		if strings.HasPrefix(m.ID, ModelPrefix) || strings.HasPrefix(m.ID, legacyPrefix) {
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
		log.Printf("open webui: removed %s, whose namespace is gone or which is outdated", id)
	}
	return o.syncChannels(ctx, signin.Token, namespaces)
}

// AccountName is the display name of the control plane's own account. Every
// member of a channel shows in its @ list, so the name says what it is.
const AccountName = "Control plane (not the agent)"

func (o *OpenWebUI) syncProfile(ctx context.Context, token string) error {
	var me struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if err := o.call(ctx, token, http.MethodGet, "/api/v1/auths/", nil, &me); err != nil {
		return err
	}
	o.accountID = me.ID
	if me.Name == AccountName {
		return nil
	}
	return o.call(ctx, token, http.MethodPost, "/api/v1/auths/update/profile", map[string]string{"name": AccountName, "profile_image_url": "/user.png"}, nil)
}

// HowToMarker starts the pinned message that explains a channel.
const HowToMarker = "**How to ask the agent of this namespace**"

func (o *OpenWebUI) howTo(ns Namespace) string {
	return fmt.Sprintf(`%[2]s

Type `+"`@agent`"+` and pick **%[1]s** from the list, then your question. For example:

`+"`@%[1]s which stacks are not healthy?`"+`
`+"`@%[1]s show the last 50 log lines of grafana_grafana`"+`
`+"`@%[1]s propose a memory limit of 1Gi for loki`"+`

It answers in a thread; reply to its answer there to go on without mentioning it again. Messages that do not mention it are for the people here. Each namespace has its own agent; in this channel, use %[1]s.

It acts with your rights. Everyone may ask anything. A change it proposes becomes a pull request in Gitea, which only people allowed to merge can apply.`, AgentName(ns.Name), HowToMarker)
}

type owuiMessage struct {
	ID       string `json:"id"`
	UserID   string `json:"user_id"`
	Content  string `json:"content"`
	IsPinned bool   `json:"is_pinned"`
}

// syncHowTo keeps the how-to pinned at the top of a namespace's channel. A
// how-to someone unpinned is pinned again; a deleted one is posted again.
func (o *OpenWebUI) syncHowTo(ctx context.Context, token, channelID string, ns Namespace) error {
	ours := func(m owuiMessage) bool { return m.UserID == o.accountID && strings.HasPrefix(m.Content, HowToMarker) }
	var pinned []owuiMessage
	if err := o.call(ctx, token, http.MethodGet, "/api/v1/channels/"+channelID+"/messages/pinned", nil, &pinned); err != nil {
		return err
	}
	for _, m := range pinned {
		if ours(m) {
			if m.Content != o.howTo(ns) {
				return o.call(ctx, token, http.MethodPost, "/api/v1/channels/"+channelID+"/messages/"+m.ID+"/update", map[string]string{"content": o.howTo(ns)}, nil)
			}
			return nil
		}
	}
	var recent []owuiMessage
	if err := o.call(ctx, token, http.MethodGet, "/api/v1/channels/"+channelID+"/messages?skip=0&limit=200", nil, &recent); err != nil {
		return err
	}
	id := ""
	for _, m := range recent {
		if ours(m) {
			id = m.ID
			break
		}
	}
	if id == "" {
		var posted owuiMessage
		if err := o.call(ctx, token, http.MethodPost, "/api/v1/channels/"+channelID+"/messages/post", map[string]string{"content": o.howTo(ns)}, &posted); err != nil {
			return err
		}
		id = posted.ID
	}
	if err := o.call(ctx, token, http.MethodPost, "/api/v1/channels/"+channelID+"/messages/"+id+"/pin", map[string]bool{"is_pinned": true}, nil); err != nil {
		return err
	}
	log.Printf("open webui: pinned the how-to in the channel of namespace %s", ns.Name)
	return nil
}

// ChannelMarker in a channel's meta names the namespace it belongs to.
const ChannelMarker = "homecloud_namespace"

type owuiChannel struct {
	ID          string         `json:"id"`
	Type        *string        `json:"type"`
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Meta        map[string]any `json:"meta"`
}

func (o *OpenWebUI) channelDescription(ns Namespace) string {
	return fmt.Sprintf("Team chat of the namespace %s. Ask its agent with @%s; see the pinned message.", ns.Name, AgentName(ns.Name))
}

var everyoneReadsAndWrites = []any{
	map[string]any{"principal_type": "user", "principal_id": "*", "permission": "read"},
	map[string]any{"principal_type": "user", "principal_id": "*", "permission": "write"},
}

// syncChannels keeps one channel per namespace, as it was made.
func (o *OpenWebUI) syncChannels(ctx context.Context, token string, namespaces []Namespace) error {
	var all []owuiChannel
	if err := o.call(ctx, token, http.MethodGet, "/api/v1/channels/list", nil, &all); err != nil {
		return err
	}
	have := map[string]owuiChannel{}
	for _, c := range all {
		if ns, ok := c.Meta[ChannelMarker].(string); ok {
			have[ns] = c
		}
	}
	for _, ns := range namespaces {
		meta := map[string]any{ChannelMarker: ns.Name}
		cur, ok := have[ns.Name]
		delete(have, ns.Name)
		switch {
		case !ok:
			form := map[string]any{"name": ns.Name, "description": o.channelDescription(ns), "meta": meta, "access_grants": everyoneReadsAndWrites}
			if err := o.call(ctx, token, http.MethodPost, "/api/v1/channels/create", form, &cur); err != nil {
				return err
			}
			log.Printf("open webui: added the channel of namespace %s", ns.Name)
		case cur.Name != ns.Name || cur.Description != o.channelDescription(ns):
			form := map[string]any{"name": ns.Name, "description": o.channelDescription(ns), "meta": meta}
			if err := o.call(ctx, token, http.MethodPost, "/api/v1/channels/"+cur.ID+"/update", form, nil); err != nil {
				return err
			}
			if err := o.call(ctx, token, http.MethodPost, "/api/v1/channels/"+cur.ID+"/access/update", map[string]any{"access_grants": everyoneReadsAndWrites}, nil); err != nil {
				return err
			}
			log.Printf("open webui: put back the channel of namespace %s", ns.Name)
		}
		if err := o.syncHowTo(ctx, token, cur.ID, ns); err != nil {
			return err
		}
	}
	for ns, c := range have {
		if err := o.call(ctx, token, http.MethodDelete, "/api/v1/channels/"+c.ID+"/delete", nil, nil); err != nil {
			return err
		}
		log.Printf("open webui: removed the channel of namespace %s, which is gone", ns)
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
