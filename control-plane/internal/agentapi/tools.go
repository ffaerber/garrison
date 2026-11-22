package agentapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"garrison/control-plane/internal/gateway"
	"garrison/control-plane/internal/sandbox"
)

// Gateway calls the gateway's MCP tools as a person, for one agent: the
// person's Keycloak token goes along, and the gateway serves the agent only
// its own tools and acts with the person's rights.
type Gateway struct {
	URL        string // e.g. http://gateway:8080/mcp
	ServiceKey string // gateway.ServiceKey: says what started a run, and runs without a person
	HTTP       *http.Client
}

// AgentHeader names the agent to the gateway.
const AgentHeader = "X-Garrison-Agent"

// As is whom a call to the gateway acts for: the person whose token it is,
// or, without one, a routine's owner, with the agent's own account.
type As struct {
	Token     string
	StartedBy string // person, agent <id>, routine <id>/<name>
	Owner     string // a run without a person: the routine's owner
	Merge     string // the merge mode of the chain a routine runs, if any
	Answered  bool   // the run went on after its owner's answer
}

// AsOf is whom a sandbox's calls act for.
func AsOf(c sandbox.Caller) As {
	return As{Token: c.Token, StartedBy: c.Run.StartedBy, Owner: c.Run.Owner, Merge: c.Run.Merge, Answered: c.Run.Answered}
}

func (g *Gateway) rpc(ctx context.Context, as As, agent, method string, params, out any) error {
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if as.Token != "" {
		req.Header.Set("Authorization", "Bearer "+as.Token)
	} else if as.Owner != "" {
		req.Header.Set(gateway.OwnerHeader, as.Owner)
	}
	if g.ServiceKey != "" {
		req.Header.Set(gateway.ServiceHeader, g.ServiceKey)
		if as.StartedBy != "" {
			req.Header.Set(gateway.StartedByHeader, as.StartedBy)
		}
		if as.Merge != "" {
			v := as.Merge
			if as.Answered {
				v += " " + gateway.MergeAnswered
			}
			req.Header.Set(gateway.MergeHeader, v)
		}
	}
	req.Header.Set(AgentHeader, agent)
	client := g.HTTP
	if client == nil {
		client = &http.Client{Timeout: 2 * time.Minute}
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("gateway: %w", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("gateway: %s: %s", resp.Status, strings.TrimSpace(string(data)))
	}
	var r struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(data, &r); err != nil {
		return fmt.Errorf("gateway: %w", err)
	}
	if r.Error != nil {
		return fmt.Errorf("gateway: %s", r.Error.Message)
	}
	return json.Unmarshal(r.Result, out)
}

// Tools lists the agent's tools, as functions for the LLM.
func (g *Gateway) Tools(ctx context.Context, as As, agent string) ([]Tool, error) {
	var out struct {
		Tools []struct {
			Name        string         `json:"name"`
			Description string         `json:"description"`
			InputSchema map[string]any `json:"inputSchema"`
		} `json:"tools"`
	}
	if err := g.rpc(ctx, as, agent, "tools/list", map[string]any{}, &out); err != nil {
		return nil, err
	}
	tools := make([]Tool, 0, len(out.Tools))
	for _, t := range out.Tools {
		var tool Tool
		tool.Type = "function"
		tool.Function.Name = t.Name
		tool.Function.Description = t.Description
		tool.Function.Parameters = t.InputSchema
		tools = append(tools, tool)
	}
	return tools, nil
}

// Call runs one tool; a refusal or failure comes back as text for the
// model to read, with isError set.
func (g *Gateway) Call(ctx context.Context, as As, agent, name, arguments string) (text string, isError bool, err error) {
	args := map[string]any{}
	if strings.TrimSpace(arguments) != "" {
		if err := json.Unmarshal([]byte(arguments), &args); err != nil {
			return "error: the arguments are not a JSON object: " + err.Error(), true, nil
		}
	}
	var out struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := g.rpc(ctx, as, agent, "tools/call", map[string]any{"name": name, "arguments": args}, &out); err != nil {
		return "", false, err
	}
	var b strings.Builder
	for _, c := range out.Content {
		b.WriteString(c.Text)
	}
	return b.String(), out.IsError, nil
}
