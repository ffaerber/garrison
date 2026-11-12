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
)

// Gateway calls the gateway's MCP tools as a person, for one agent: the
// person's Keycloak token goes along, and the gateway serves the agent only
// its own tools and acts with the person's rights.
type Gateway struct {
	URL  string // e.g. http://control-plane_gateway:8080/mcp
	HTTP *http.Client
}

// AgentHeader names the agent to the gateway.
const AgentHeader = "X-Homecloud-Agent"

func (g *Gateway) rpc(ctx context.Context, token, agent, method string, params, out any) error {
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+token)
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
func (g *Gateway) Tools(ctx context.Context, token, agent string) ([]Tool, error) {
	var out struct {
		Tools []struct {
			Name        string         `json:"name"`
			Description string         `json:"description"`
			InputSchema map[string]any `json:"inputSchema"`
		} `json:"tools"`
	}
	if err := g.rpc(ctx, token, agent, "tools/list", map[string]any{}, &out); err != nil {
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
func (g *Gateway) Call(ctx context.Context, token, agent, name, arguments string) (text string, isError bool, err error) {
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
	if err := g.rpc(ctx, token, agent, "tools/call", map[string]any{"name": name, "arguments": args}, &out); err != nil {
		return "", false, err
	}
	var b strings.Builder
	for _, c := range out.Content {
		b.WriteString(c.Text)
	}
	return b.String(), out.IsError, nil
}
