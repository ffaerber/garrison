// Package gateway is the gateway: an MCP server that gives chat
// agents (Open WebUI, one per namespace) tools to look at the system and to
// propose changes, acting as the person who asks; and the sync that keeps
// one such agent per namespace in Open WebUI.
package gateway

import (
	"context"
	"crypto/hmac"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"slices"
	"strings"

	"github.com/coreos/go-oidc/v3/oidc"

	"garrison/control-plane/internal/agents"
	"garrison/control-plane/internal/events"
)

// Verifier turns a bearer token into the person it was issued to.
type Verifier interface {
	Verify(ctx context.Context, token string) (Person, error)
}

// KeycloakVerifier accepts access tokens of the installation's Keycloak realm
// that were issued to one of the allowed clients, e.g. Open WebUI, which
// forwards each person's own token (its "OAuth" tool server auth).
type KeycloakVerifier struct {
	verifier *oidc.IDTokenVerifier
	clients  []string
}

const adminGroup = "root-admins"

// NewKeycloakVerifier discovers the realm. ctx carries the HTTP client that
// trusts the installation's certificate (oidc.ClientContext).
func NewKeycloakVerifier(ctx context.Context, issuer string, clients []string) (*KeycloakVerifier, error) {
	provider, err := oidc.NewProvider(ctx, issuer)
	if err != nil {
		return nil, err
	}
	// Access tokens carry the client in azp, not aud; that is checked below.
	return &KeycloakVerifier{verifier: provider.Verifier(&oidc.Config{SkipClientIDCheck: true}), clients: clients}, nil
}

func (k *KeycloakVerifier) Verify(ctx context.Context, raw string) (Person, error) {
	tok, err := k.verifier.Verify(ctx, raw)
	if err != nil {
		return Person{}, err
	}
	var c struct {
		Azp      string   `json:"azp"`
		Type     string   `json:"typ"`
		Username string   `json:"preferred_username"`
		Email    string   `json:"email"`
		Groups   []string `json:"groups"`
	}
	if err := tok.Claims(&c); err != nil {
		return Person{}, err
	}
	if c.Type != "Bearer" {
		return Person{}, fmt.Errorf("not an access token")
	}
	if !slices.Contains(k.clients, c.Azp) {
		return Person{}, fmt.Errorf("token issued to %q, not an allowed client", c.Azp)
	}
	if !userName.MatchString(c.Username) {
		return Person{}, fmt.Errorf("token has no usable username")
	}
	return Person{Name: c.Username, Email: c.Email, Admin: slices.Contains(c.Groups, adminGroup)}, nil
}

// MCP serves the tools over MCP's Streamable HTTP transport, answering each
// JSON-RPC request with a JSON response. It keeps no sessions.
//
// Each agent gets only the tools its agent.yaml lists. Open WebUI names the
// agent in the header X-Garrison-Agent (its model id); /mcp/<namespace>/<agent>
// names it in the path. /mcp without the header serves every base tool.
type MCP struct {
	Tools    *Tools
	Verifier Verifier
	Agents   *Catalog
	// ServiceKey lets the agent service say what started a run and run an
	// agent without a person (ServiceKey()). Empty: neither.
	ServiceKey string
	// Events, if set, gets every call that changes something and every
	// refusal, for the dashboard's feed.
	Events events.Recorder
}

// Headers of the agent service, believed only with its key.
const (
	ServiceHeader   = "X-Garrison-Service"
	StartedByHeader = "X-Garrison-Started-By"
	OwnerHeader     = "X-Garrison-Owner" // a run without a person: the routine's owner
)

// AgentHeader names the agent a request is for.
const AgentHeader = "X-Garrison-Agent"

// agentFor finds the agent a request names; nil for every base tool. An
// agent nobody defined, e.g. a model an admin made by hand, gets nothing.
func (m *MCP) agentFor(r *http.Request) (*agents.Agent, bool) {
	id, inPath := strings.CutPrefix(r.URL.Path, "/mcp/")
	if !inPath {
		if _, named := r.Header[http.CanonicalHeaderKey(AgentHeader)]; !named {
			return nil, true
		}
		id = r.Header.Get(AgentHeader)
	}
	if m.Agents == nil {
		return nil, false
	}
	a, ok := m.Agents.Lookup(r.Context(), id)
	return &a, ok
}

var supportedVersions = []string{"2025-06-18", "2025-03-26", "2024-11-05"}

// maxRequest is the largest request: room for the largest patch
// propose_patch takes, as JSON, which escapes some of it. The agent service
// lets a sandbox send as much.
const maxRequest = 2 * MaxPatch

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (m *MCP) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	agent, ok := m.agentFor(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		// No server-initiated stream and no sessions to end.
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	person, ok := m.person(w, r, agent)
	if !ok {
		return
	}
	if allowed, err := m.Tools.Gitea.CanUse(r.Context(), agent, person); err != nil {
		http.Error(w, "cannot read the namespace's members", http.StatusBadGateway)
		return
	} else if !allowed {
		http.Error(w, fmt.Sprintf("%s may not use the agent %s: in %s it is for the people of %s", person.Name, agent.Name, agent.Namespace, agent.Namespace), http.StatusForbidden)
		return
	}
	var req rpcRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequest)).Decode(&req); err != nil || req.JSONRPC != "2.0" || req.Method == "" {
		writeRPC(w, nil, nil, &rpcError{Code: -32700, Message: "parse error"})
		return
	}
	if len(req.ID) == 0 { // a notification, e.g. notifications/initialized
		w.WriteHeader(http.StatusAccepted)
		return
	}
	result, rerr := m.handle(r.Context(), person, agent, req)
	writeRPC(w, req.ID, result, rerr)
}

// person is who a request acts for: the person whose Keycloak token it
// carries, or, from the agent service, a routine's owner in a run without a
// person, which then acts with the agent's own account.
func (m *MCP) person(w http.ResponseWriter, r *http.Request, agent *agents.Agent) (Person, bool) {
	service := m.ServiceKey != "" && hmac.Equal([]byte(r.Header.Get(ServiceHeader)), []byte(m.ServiceKey))
	raw, hasToken := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if service && (!hasToken || raw == "") && r.Header.Get(OwnerHeader) != "" {
		owner := r.Header.Get(OwnerHeader)
		if agent == nil || !userName.MatchString(owner) {
			http.Error(w, "a run without a person needs an agent and its owner", http.StatusBadRequest)
			return Person{}, false
		}
		return Person{Name: AgentAccount(agent), Unattended: true, Owner: owner, StartedBy: startedBy(r)}, true
	}
	if !hasToken || raw == "" {
		w.Header().Set("WWW-Authenticate", `Bearer realm="garrison"`)
		http.Error(w, "a Keycloak access token is required", http.StatusUnauthorized)
		return Person{}, false
	}
	p, err := m.Verifier.Verify(r.Context(), raw)
	if err != nil {
		log.Printf("mcp: refused token: %v", err)
		w.Header().Set("WWW-Authenticate", `Bearer realm="garrison", error="invalid_token"`)
		http.Error(w, "invalid token", http.StatusUnauthorized)
		return Person{}, false
	}
	p.Token = raw
	p.StartedBy = "person"
	if service {
		p.StartedBy = startedBy(r)
	}
	return p, true
}

var startedByValue = regexp.MustCompile(`^(person|agent [A-Za-z0-9._/-]{1,200}|routine [A-Za-z0-9._/-]{1,200})$`)

func startedBy(r *http.Request) string {
	if v := r.Header.Get(StartedByHeader); startedByValue.MatchString(v) {
		return v
	}
	return "person"
}

func (m *MCP) handle(ctx context.Context, p Person, agent *agents.Agent, req rpcRequest) (any, *rpcError) {
	switch req.Method {
	case "initialize":
		var params struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		json.Unmarshal(req.Params, &params)
		version := supportedVersions[0]
		if slices.Contains(supportedVersions, params.ProtocolVersion) {
			version = params.ProtocolVersion
		}
		return map[string]any{
			"protocolVersion": version,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]any{"name": "garrison-gateway", "version": "1"},
			"instructions":    "Tools to look at the home cloud and to propose changes as the person asking. Nothing here merges or deploys: a person merges proposed changes in Gitea.",
		}, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		return map[string]any{"tools": SpecsFor(agent)}, nil
	case "tools/call":
		var params struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &params); err != nil {
			return nil, &rpcError{Code: -32602, Message: "invalid params"}
		}
		if !slices.ContainsFunc(SpecsFor(agent), func(s Spec) bool { return s.Name == params.Name }) {
			return nil, &rpcError{Code: -32602, Message: "unknown tool " + params.Name}
		}
		out, err := m.Tools.Call(ctx, p, agent, params.Name, params.Arguments)
		m.record(ctx, p, agent, params.Name, params.Arguments, out, err)
		if err != nil {
			msg := err.Error()
			if errors.Is(err, ErrForbidden) {
				msg = fmt.Sprintf("not allowed: %s may not do this", p.Name)
			}
			log.Printf("mcp: %s: %s: %s [%s]: %s", who(p), agentID(agent), params.Name, p.StartedBy, msg)
			return map[string]any{"content": []map[string]any{{"type": "text", "text": "error: " + msg}}, "isError": true}, nil
		}
		log.Printf("mcp: %s: %s: %s [%s]", who(p), agentID(agent), params.Name, p.StartedBy)
		return map[string]any{"content": []map[string]any{{"type": "text", "text": out}}}, nil
	}
	return nil, &rpcError{Code: -32601, Message: "method not found: " + req.Method}
}

// who names whom a request acts for, in the log.
func who(p Person) string {
	if p.Unattended {
		return p.Owner + " (nobody watching, as " + p.Name + ")"
	}
	return p.Name
}

func agentID(a *agents.Agent) string {
	if a == nil {
		return "-"
	}
	return a.ID()
}

func writeRPC(w http.ResponseWriter, id json.RawMessage, result any, rerr *rpcError) {
	resp := map[string]any{"jsonrpc": "2.0", "id": id}
	if id == nil {
		resp["id"] = nil
	}
	if rerr != nil {
		resp["error"] = rerr
	} else {
		resp["result"] = result
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}
