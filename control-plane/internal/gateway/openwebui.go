package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"homecloud/control-plane/internal/agents"
	"homecloud/control-plane/internal/hooks"
)

// OpenWebUI keeps, for every agent of every namespace, a model entry in
// Open WebUI, and for every namespace a channel. The agents are the models of
// Open WebUI's one connection, the agent service, which runs them; the entry,
// named <namespace>/<agent>, lets everyone signed in use the agent and says
// what it does.
// The channel is the namespace's team chat: everyone signed in reads and
// posts there, and mentioning an agent makes it answer, with the tools
// acting as the person who mentioned it. Only admins may change a channel,
// and the sync puts back what it owns. It signs in as the local admin Open
// WebUI creates from its environment at first start.
type OpenWebUI struct {
	// Wake, when set, starts a sync before the interval is over.
	Wake     <-chan struct{}
	URL      string // e.g. http://openwebui_openwebui:8080
	Email    string
	Password string
	Domain   string
	Gitea    *Gitea
	Agents   *Catalog // filled with the agents read, for the MCP endpoints
	HTTP     *http.Client

	mu            sync.Mutex        // guards token: posts come from other goroutines
	token         string            // kept between syncs: Open WebUI limits sign-ins
	channelAccess map[string]string // channel -> the access last set; Open WebUI returns it in its own shape
	accountID     string            // the gateway's own Open WebUI account
	problems      map[string]bool   // agent problems already logged
}

// AgentMarker in a model's meta names the agent it is; the sync owns such
// models.
const AgentMarker = "homecloud_agent"

// legacyPrefixes mark agents of earlier versions, which the sync removes.
var legacyPrefixes = []string{"agent-", "ns-"}

type owuiModel struct {
	ID           string         `json:"id"`
	BaseModelID  *string        `json:"base_model_id"` // nil: a model of a connection, not a preset
	Name         string         `json:"name"`
	Meta         map[string]any `json:"meta"`
	Params       map[string]any `json:"params"`
	AccessGrants []any          `json:"access_grants"`
	IsActive     bool           `json:"is_active"`
}

// AccessMarker in a model's meta records whom the sync let use it, as Open
// WebUI returns access grants in its own shape.
const AccessMarker = "homecloud_access"

// want is the model entry of an agent. A private agent (AgentPrivate) goes
// only to its namespace's group (groups); root admins are Open WebUI's
// admins and see every agent.
func (o *OpenWebUI) want(a agents.Agent, groups map[string]string, private bool) owuiModel {
	desc := a.Description
	if desc == "" {
		desc = "Agent of the namespace " + a.Namespace
	}
	access, grants := "everyone", []any{map[string]any{"principal_type": "user", "principal_id": "*", "permission": "read"}}
	if private {
		desc += " (works on private repos: for " + a.Namespace + "'s people)"
		access, grants = "admins", []any{}
		if id := groups[a.Namespace]; id != "" {
			access, grants = "group:"+id, []any{map[string]any{"principal_type": "group", "principal_id": id, "permission": "read"}}
		}
	}
	return owuiModel{
		ID:   a.ID(),
		Name: a.ID(),
		Meta: map[string]any{
			"description": desc,
			AgentMarker:   a.ID(),
			AccessMarker:  access,
		},
		Params:       map[string]any{},
		AccessGrants: grants,
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

// Sync makes Open WebUI's agents and channels match the
// namespaces and their agents in Gitea.
func (o *OpenWebUI) Sync(ctx context.Context) error {
	if o.channelAccess == nil {
		o.channelAccess = map[string]string{}
	}
	token, err := o.session(ctx)
	if err != nil {
		return err
	}
	signin := struct{ Token string }{token}
	if err := o.syncProfile(ctx, signin.Token); err != nil {
		return err
	}
	namespaces, err := o.Gitea.Namespaces(ctx, "")
	if err != nil {
		return err
	}
	var list []agents.Agent
	byNS := map[string][]agents.Agent{}
	seen := map[string]bool{}
	for _, ns := range namespaces {
		found, problems, err := o.Gitea.Agents(ctx, ns.Name, "main")
		if err != nil {
			return fmt.Errorf("agents of %s: %w", ns.Name, err)
		}
		for _, p := range problems {
			key := ns.Name + "/config: " + p
			if o.problems == nil {
				o.problems = map[string]bool{}
			}
			if !o.problems[key] {
				log.Printf("open webui: left out: %s", key)
			}
			seen[key] = true
		}
		list = append(list, found...)
		byNS[ns.Name] = found
	}
	o.problems = seen
	if o.Agents != nil {
		o.Agents.Set(list)
	}
	groups, err := o.syncGroups(ctx, signin.Token, namespaces)
	if err != nil {
		return err
	}
	private := map[string]bool{}
	for _, a := range list {
		if private[a.ID()], err = o.Gitea.AgentPrivate(ctx, &a); err != nil {
			return err
		}
	}
	if err := o.syncModels(ctx, signin.Token, list, groups, private); err != nil {
		return err
	}
	return o.syncChannels(ctx, signin.Token, namespaces, byNS, groups)
}

// session returns a token, signing in only when the kept one no longer
// works: Open WebUI limits sign-ins, and webhooks start many syncs.
func (o *OpenWebUI) session(ctx context.Context) (string, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.token != "" && o.call(ctx, o.token, http.MethodGet, "/api/v1/auths/", nil, nil) == nil {
		return o.token, nil
	}
	var signin struct {
		Token string `json:"token"`
	}
	if err := o.call(ctx, "", http.MethodPost, "/api/v1/auths/signin", map[string]string{"email": o.Email, "password": o.Password}, &signin); err != nil {
		o.token = ""
		return "", err
	}
	o.token = signin.Token
	return o.token, nil
}

func (o *OpenWebUI) syncModels(ctx context.Context, token string, list []agents.Agent, groups map[string]string, private map[string]bool) error {
	var all []owuiModel
	if err := o.call(ctx, token, http.MethodGet, "/api/v1/models/all", nil, &all); err != nil {
		return err
	}
	have := map[string]owuiModel{}
	for _, m := range all {
		_, ours := m.Meta[AgentMarker]
		if ours || slices.ContainsFunc(legacyPrefixes, func(p string) bool { return strings.HasPrefix(m.ID, p) }) {
			have[m.ID] = m
		}
	}
	for _, a := range list {
		want := o.want(a, groups, private[a.ID()])
		cur, ok := have[want.ID]
		delete(have, want.ID)
		switch {
		case !ok:
			if err := o.call(ctx, token, http.MethodPost, "/api/v1/models/create", want, nil); err != nil {
				return err
			}
			log.Printf("open webui: added the agent %s", want.ID)
		case cur.BaseModelID != nil:
			// A preset of an earlier version: made again as the connection's model.
			if err := o.call(ctx, token, http.MethodPost, "/api/v1/models/model/delete", map[string]string{"id": cur.ID}, nil); err != nil {
				return err
			}
			if err := o.call(ctx, token, http.MethodPost, "/api/v1/models/create", want, nil); err != nil {
				return err
			}
			log.Printf("open webui: made the agent %s a model of the agent service", want.ID)
		case !same(cur, want):
			if err := o.call(ctx, token, http.MethodPost, "/api/v1/models/model/update", want, nil); err != nil {
				return err
			}
			// The update keeps the access grants: an agent whose repo went
			// private would stay visible to everyone.
			if err := o.call(ctx, token, http.MethodPost, "/api/v1/models/model/access/update", map[string]any{"id": want.ID, "access_grants": want.AccessGrants}, nil); err != nil {
				return err
			}
			log.Printf("open webui: updated the agent %s", want.ID)
		}
	}
	for id := range have {
		if err := o.call(ctx, token, http.MethodPost, "/api/v1/models/model/delete", map[string]string{"id": id}, nil); err != nil {
			return err
		}
		log.Printf("open webui: removed %s, whose folder or namespace is gone or which is outdated", id)
	}
	return nil
}

// AccountName is the display name of the gateway's own account. Every
// member of a channel shows in its @ list, so the name says what it is.
const AccountName = "Gateway (not the agent)"

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

func (o *OpenWebUI) howTo(ns Namespace, list []agents.Agent) string {
	if len(list) == 0 {
		return HowToMarker + "\n\nThis namespace has no agents. Add one as a folder under agents/ in its repo config."
	}
	var b strings.Builder
	b.WriteString(HowToMarker + "\n\nType `@` and pick an agent from the list, then your question. The agents of this namespace:\n\n")
	example := list[0]
	for _, a := range list {
		desc := a.Description
		if desc == "" {
			desc = "no description"
		}
		fmt.Fprintf(&b, "- **%s**: %s\n", a.ID(), desc)
		if a.Name == agents.OperatorName {
			example = a
		}
	}
	fmt.Fprintf(&b, `
For example:

`+"`@%[1]s which stacks are not healthy?`"+`
`+"`@%[1]s show the last 50 log lines of grafana_grafana`"+`

It answers in a thread; reply to its answer there to go on without mentioning it again. Messages that do not mention an agent are for the people here.

An agent acts with your rights. A change it proposes becomes a pull request in Gitea, which only people allowed to merge can apply. The agents are folders under agents/ in the repo config.`, example.ID())
	if ns.Name != SystemNamespace {
		b.WriteString("\n\nOnly the people of " + ns.Name + " read this channel. Others can talk to its agents that work on public repos in a private chat.")
	}
	return b.String()
}

type owuiMessage struct {
	ID       string `json:"id"`
	UserID   string `json:"user_id"`
	Content  string `json:"content"`
	IsPinned bool   `json:"is_pinned"`
}

// syncHowTo keeps the how-to pinned at the top of a namespace's channel. A
// how-to someone unpinned is pinned again; a deleted one is posted again.
func (o *OpenWebUI) syncHowTo(ctx context.Context, token, channelID string, ns Namespace, list []agents.Agent) error {
	ours := func(m owuiMessage) bool { return m.UserID == o.accountID && strings.HasPrefix(m.Content, HowToMarker) }
	var pinned []owuiMessage
	if err := o.call(ctx, token, http.MethodGet, "/api/v1/channels/"+channelID+"/messages/pinned", nil, &pinned); err != nil {
		return err
	}
	for _, m := range pinned {
		if ours(m) {
			if m.Content != o.howTo(ns, list) {
				return o.call(ctx, token, http.MethodPost, "/api/v1/channels/"+channelID+"/messages/"+m.ID+"/update", map[string]string{"content": o.howTo(ns, list)}, nil)
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
		if err := o.call(ctx, token, http.MethodPost, "/api/v1/channels/"+channelID+"/messages/post", map[string]string{"content": o.howTo(ns, list)}, &posted); err != nil {
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
	ID           string         `json:"id"`
	Type         *string        `json:"type"`
	Name         string         `json:"name"`
	Description  string         `json:"description"`
	Meta         map[string]any `json:"meta"`
	AccessGrants []any          `json:"access_grants,omitempty"`
}

func (o *OpenWebUI) channelDescription(ns Namespace) string {
	if ns.Name == SystemNamespace {
		return "Chat of the platform, for everyone signed in. Ask its agents with @system/<agent>; see the pinned message."
	}
	return fmt.Sprintf("Team chat of the namespace %s: only its people read it. Ask its agents with @%s/<agent>; see the pinned message.", ns.Name, ns.Name)
}

var everyoneReadsAndWrites = []any{
	map[string]any{"principal_type": "user", "principal_id": "*", "permission": "read"},
	map[string]any{"principal_type": "user", "principal_id": "*", "permission": "write"},
}

// channelAccess is who reads and writes a namespace's channel: its people,
// by its Open WebUI group (root admins, Open WebUI's admins, read every
// channel). system's is for everyone signed in: it has no people of its own,
// and its repos and agents serve everyone.
func channelAccess(ns, group string) []any {
	if ns == SystemNamespace {
		return everyoneReadsAndWrites
	}
	if group == "" {
		return []any{}
	}
	return []any{
		map[string]any{"principal_type": "group", "principal_id": group, "permission": "read"},
		map[string]any{"principal_type": "group", "principal_id": group, "permission": "write"},
	}
}

// legacyTeamMarker marked the team channels of an earlier version, which
// the sync removes: a namespace's channel is for its people now.
const legacyTeamMarker = "homecloud_team"

// syncChannels keeps one channel per namespace, as it was made, for the
// namespace's people.
func (o *OpenWebUI) syncChannels(ctx context.Context, token string, namespaces []Namespace, byNS map[string][]agents.Agent, groups map[string]string) error {
	var all []owuiChannel
	if err := o.call(ctx, token, http.MethodGet, "/api/v1/channels/list", nil, &all); err != nil {
		return err
	}
	have := map[string]owuiChannel{}
	for _, c := range all {
		if ns, ok := c.Meta[ChannelMarker].(string); ok {
			have[ns] = c
		}
		if ns, ok := c.Meta[legacyTeamMarker].(string); ok {
			if err := o.call(ctx, token, http.MethodDelete, "/api/v1/channels/"+c.ID+"/delete", nil, nil); err != nil {
				return err
			}
			log.Printf("open webui: removed the team channel of namespace %s: its channel is for its people now", ns)
		}
	}
	for _, ns := range namespaces {
		meta := map[string]any{ChannelMarker: ns.Name}
		access := channelAccess(ns.Name, groups[ns.Name])
		key, _ := json.Marshal(access)
		cur, ok := have[ns.Name]
		delete(have, ns.Name)
		switch {
		case !ok:
			form := map[string]any{"name": ns.Name, "description": o.channelDescription(ns), "meta": meta, "access_grants": access}
			if err := o.call(ctx, token, http.MethodPost, "/api/v1/channels/create", form, &cur); err != nil {
				return err
			}
			o.channelAccess[cur.ID] = string(key)
			log.Printf("open webui: added the channel of namespace %s", ns.Name)
		case cur.Name != ns.Name || cur.Description != o.channelDescription(ns) || o.channelAccess[cur.ID] != string(key):
			form := map[string]any{"name": ns.Name, "description": o.channelDescription(ns), "meta": meta}
			if err := o.call(ctx, token, http.MethodPost, "/api/v1/channels/"+cur.ID+"/update", form, nil); err != nil {
				return err
			}
			if err := o.call(ctx, token, http.MethodPost, "/api/v1/channels/"+cur.ID+"/access/update", map[string]any{"access_grants": access}, nil); err != nil {
				return err
			}
			o.channelAccess[cur.ID] = string(key)
			log.Printf("open webui: set the channel of namespace %s", ns.Name)
		}
		if err := o.syncHowTo(ctx, token, cur.ID, ns, byNS[ns.Name]); err != nil {
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

// same compares the fields the sync owns. Access grants are compared by
// the marker the sync writes with them: Open WebUI returns them in its own
// shape.
func same(a, b owuiModel) bool {
	if a.Name != b.Name || (a.BaseModelID == nil) != (b.BaseModelID == nil) || !a.IsActive {
		return false
	}
	ja, _ := json.Marshal([]any{a.Meta["description"], a.Meta[AgentMarker], a.Meta[AccessMarker]})
	jb, _ := json.Marshal([]any{b.Meta["description"], b.Meta[AgentMarker], b.Meta[AccessMarker]})
	return bytes.Equal(ja, jb)
}

// Run syncs now and then every interval, logging failures; Open WebUI may
// still be starting.
func (o *OpenWebUI) Run(ctx context.Context, interval time.Duration) {
	for {
		if err := o.Sync(ctx); err != nil {
			log.Printf("open webui sync: %v", err)
		}
		if !hooks.Sleep(ctx, interval, o.Wake) {
			return
		}
	}
}

// GroupPrefix names the Open WebUI group of a namespace's people:
// "homecloud <namespace>".
const GroupPrefix = "homecloud "

type owuiGroup struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// syncGroups keeps one Open WebUI group per namespace with the people of its
// members.yaml who have signed in to Open WebUI, matched by email, so that
// agents visible to members show only to them. It returns the group ids by
// namespace. system has none: its people are root admins, whom Open WebUI
// shows everything.
func (o *OpenWebUI) syncGroups(ctx context.Context, token string, namespaces []Namespace) (map[string]string, error) {
	var groups []owuiGroup
	if err := o.call(ctx, token, http.MethodGet, "/api/v1/groups/", nil, &groups); err != nil {
		return nil, err
	}
	have := map[string]string{}
	for _, g := range groups {
		if ns, ok := strings.CutPrefix(g.Name, GroupPrefix); ok {
			have[ns] = g.ID
		}
	}
	var users struct {
		Users []struct {
			ID    string `json:"id"`
			Email string `json:"email"`
		} `json:"users"`
	}
	if err := o.call(ctx, token, http.MethodGet, "/api/v1/users/all", nil, &users); err != nil {
		return nil, err
	}
	byEmail := map[string]string{}
	for _, u := range users.Users {
		byEmail[strings.ToLower(u.Email)] = u.ID
	}
	out := map[string]string{}
	for _, ns := range namespaces {
		if ns.Name == SystemNamespace {
			continue
		}
		id, ok := have[ns.Name]
		delete(have, ns.Name)
		if !ok {
			var g owuiGroup
			if err := o.call(ctx, token, http.MethodPost, "/api/v1/groups/create", map[string]any{
				"name": GroupPrefix + ns.Name, "description": "The people of the namespace " + ns.Name + ", from members.yaml in its repo config; kept by the gateway.",
			}, &g); err != nil {
				return nil, err
			}
			id = g.ID
			log.Printf("open webui: added the group of namespace %s", ns.Name)
		}
		out[ns.Name] = id
		var raw string
		if _, err := o.Gitea.do(ctx, "", http.MethodGet, "/repos/"+ns.Name+"/"+ConfigRepo+"/raw/"+MembersFile+"?ref=main", nil, &raw); err != nil {
			continue // no roster yet: keep the group as it is
		}
		roster, err := ParseRoster([]byte(raw))
		if err != nil {
			continue
		}
		var want []string
		for _, name := range slices.Concat(roster.Admins, roster.Members) {
			var u struct {
				Email string `json:"email"`
			}
			if _, err := o.Gitea.do(ctx, "", http.MethodGet, "/users/"+name, nil, &u); err != nil {
				continue // not signed in yet
			}
			if uid := byEmail[strings.ToLower(u.Email)]; uid != "" {
				want = append(want, uid)
			}
		}
		var members []struct {
			ID string `json:"id"`
		}
		if err := o.call(ctx, token, http.MethodPost, "/api/v1/groups/id/"+id+"/users", nil, &members); err != nil {
			return nil, err
		}
		var cur, add, remove []string
		for _, m := range members {
			cur = append(cur, m.ID)
			if !slices.Contains(want, m.ID) {
				remove = append(remove, m.ID)
			}
		}
		for _, uid := range want {
			if !slices.Contains(cur, uid) {
				add = append(add, uid)
			}
		}
		if len(add) > 0 {
			if err := o.call(ctx, token, http.MethodPost, "/api/v1/groups/id/"+id+"/users/add", map[string]any{"user_ids": add}, nil); err != nil {
				return nil, err
			}
		}
		if len(remove) > 0 {
			if err := o.call(ctx, token, http.MethodPost, "/api/v1/groups/id/"+id+"/users/remove", map[string]any{"user_ids": remove}, nil); err != nil {
				return nil, err
			}
		}
		if len(add)+len(remove) > 0 {
			log.Printf("open webui: the group of namespace %s: %d added, %d removed", ns.Name, len(add), len(remove))
		}
	}
	for ns, id := range have {
		if err := o.call(ctx, token, http.MethodDelete, "/api/v1/groups/id/"+id+"/delete", nil, nil); err != nil {
			return nil, err
		}
		log.Printf("open webui: removed the group of namespace %s, which is gone", ns)
	}
	return out, nil
}
