// Package gateway is the gateway: an MCP server that gives chat
// agents (Open WebUI, one per namespace) tools to look at the system and to
// propose changes, acting as the person who asks; and the sync that keeps
// one such agent per namespace in Open WebUI.
package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"slices"
	"strings"

	"github.com/coreos/go-oidc/v3/oidc"
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
type MCP struct {
	Tools    *Tools
	Verifier Verifier
}

var supportedVersions = []string{"2025-06-18", "2025-03-26", "2024-11-05"}

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
	if r.Method != http.MethodPost {
		// No server-initiated stream and no sessions to end.
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	raw, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || raw == "" {
		w.Header().Set("WWW-Authenticate", `Bearer realm="homecloud"`)
		http.Error(w, "a Keycloak access token is required", http.StatusUnauthorized)
		return
	}
	person, err := m.Verifier.Verify(r.Context(), raw)
	if err != nil {
		log.Printf("mcp: refused token: %v", err)
		w.Header().Set("WWW-Authenticate", `Bearer realm="homecloud", error="invalid_token"`)
		http.Error(w, "invalid token", http.StatusUnauthorized)
		return
	}
	var req rpcRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil || req.JSONRPC != "2.0" || req.Method == "" {
		writeRPC(w, nil, nil, &rpcError{Code: -32700, Message: "parse error"})
		return
	}
	if len(req.ID) == 0 { // a notification, e.g. notifications/initialized
		w.WriteHeader(http.StatusAccepted)
		return
	}
	result, rerr := m.handle(r.Context(), person, req)
	writeRPC(w, req.ID, result, rerr)
}

func (m *MCP) handle(ctx context.Context, p Person, req rpcRequest) (any, *rpcError) {
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
			"serverInfo":      map[string]any{"name": "homecloud-gateway", "version": "1"},
			"instructions":    "Tools to look at the home cloud and to propose changes as the person asking. Nothing here merges or deploys: a person merges proposed changes in Gitea.",
		}, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		return map[string]any{"tools": m.Tools.Specs()}, nil
	case "tools/call":
		var params struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &params); err != nil {
			return nil, &rpcError{Code: -32602, Message: "invalid params"}
		}
		if !slices.ContainsFunc(m.Tools.Specs(), func(s Spec) bool { return s.Name == params.Name }) {
			return nil, &rpcError{Code: -32602, Message: "unknown tool " + params.Name}
		}
		out, err := m.Tools.Call(ctx, p, params.Name, params.Arguments)
		if err != nil {
			msg := err.Error()
			if errors.Is(err, ErrForbidden) {
				msg = fmt.Sprintf("not allowed: %s may not do this", p.Name)
			}
			log.Printf("mcp: %s: %s: %s", p.Name, params.Name, msg)
			return map[string]any{"content": []map[string]any{{"type": "text", "text": "error: " + msg}}, "isError": true}, nil
		}
		log.Printf("mcp: %s: %s", p.Name, params.Name)
		return map[string]any{"content": []map[string]any{{"type": "text", "text": out}}}, nil
	}
	return nil, &rpcError{Code: -32601, Message: "method not found: " + req.Method}
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
