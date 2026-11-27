package models

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
)

// The setup tests a provider before it is saved: does it answer with its
// models for this key, and does the model chosen as the default answer a
// question.

// List asks the OpenAI-compatible API at baseURL for its models, with key
// if set: GET <baseURL>/models. It returns their names, sorted.
func List(ctx context.Context, client *http.Client, baseURL, key string) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(baseURL, "/")+"/models", nil)
	if err != nil {
		return nil, err
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cannot reach %s: %w", baseURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return nil, fmt.Errorf("%s/models answered %s: %s", strings.TrimSuffix(baseURL, "/"), resp.Status, strings.TrimSpace(string(data)))
	}
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 32<<20)).Decode(&out); err != nil {
		return nil, fmt.Errorf("%s/models is not an OpenAI-style list of models: %w", strings.TrimSuffix(baseURL, "/"), err)
	}
	var ids []string
	for _, d := range out.Data {
		if d.ID != "" && modelID.MatchString("p/"+d.ID) && !slices.Contains(ids, d.ID) {
			ids = append(ids, d.ID)
		}
	}
	slices.Sort(ids)
	if len(ids) == 0 {
		return nil, fmt.Errorf("%s/models lists no models", strings.TrimSuffix(baseURL, "/"))
	}
	return ids, nil
}

// Try asks model at baseURL a short question and returns its answer, to
// tell that it works with this key.
func Try(ctx context.Context, client *http.Client, baseURL, key, model string) (string, error) {
	body, _ := json.Marshal(map[string]any{
		"model":      model,
		"messages":   []map[string]string{{"role": "user", "content": "Answer with one word: ready."}},
		"max_tokens": 200,
		"stream":     false,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(baseURL, "/")+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("cannot reach %s: %w", baseURL, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return "", &StatusError{Status: resp.StatusCode, Text: fmt.Sprintf("%s answered %s: %s", model, resp.Status, strings.TrimSpace(string(data[:min(len(data), 300)])))}
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content   string `json:"content"`
				Reasoning string `json:"reasoning_content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(data, &out); err != nil || len(out.Choices) == 0 {
		return "", fmt.Errorf("%s gave no answer", model)
	}
	answer := strings.TrimSpace(out.Choices[0].Message.Content)
	if answer == "" && strings.TrimSpace(out.Choices[0].Message.Reasoning) != "" {
		answer = "(it thought, and ran out of room before answering; it works)"
	}
	if answer == "" {
		return "", fmt.Errorf("%s gave an empty answer", model)
	}
	if len(answer) > 200 {
		answer = answer[:200] + "…"
	}
	return answer, nil
}

// StatusError is a provider's answer other than 200.
type StatusError struct {
	Status int
	Text   string
}

func (e *StatusError) Error() string { return e.Text }

// CheckKey tells whether the provider takes key, by asking one of its
// models (ids, as List gave them) a question: some providers, such as
// Ollama's cloud, list their models for anyone. Only a refusal of the key
// (401, 403) fails it; a model that is busy or slow says nothing about the key.
func CheckKey(ctx context.Context, client *http.Client, baseURL, key string, ids []string) error {
	if key == "" || len(ids) == 0 {
		return nil
	}
	_, err := Try(ctx, client, baseURL, key, ids[0])
	var se *StatusError
	if errors.As(err, &se) && (se.Status == http.StatusUnauthorized || se.Status == http.StatusForbidden) {
		return fmt.Errorf("it refuses the key: %s", se.Text)
	}
	return nil
}

// Seeded is providers.yaml and models.yaml as the setup writes them: the
// providers by name with their URLs, def the default, and every model each
// lists, turned off but for the default.
func Seeded(providers map[string]string, lists map[string][]string, def, today string) ([]byte, []byte, error) {
	var b strings.Builder
	b.WriteString("# The LLM providers of Garrison's agents: OpenAI-compatible APIs. See README.md.\n")
	b.WriteString("# Their keys are secrets of garrison, set on the dashboard's Models page; never in this file.\n")
	d, _ := json.Marshal(def)
	fmt.Fprintf(&b, "default: %s\nproviders:\n", d)
	for _, name := range sortedKeys(providers) {
		u, _ := json.Marshal(providers[name])
		fmt.Fprintf(&b, "  %s:\n    url: %s\n", name, u)
	}
	p, why := ParseProviders(b.String())
	if len(why) > 0 {
		return nil, nil, fmt.Errorf("%s", strings.Join(why, "; "))
	}
	c := Catalog{}
	for name, ids := range lists {
		for _, id := range ids {
			full := name + "/" + id
			if modelID.MatchString(full) {
				c[full] = Model{Enabled: full == def, Added: today}
			}
		}
	}
	if why := Check(p, c); len(why) > 0 {
		return nil, nil, fmt.Errorf("%s", strings.Join(why, "; "))
	}
	return []byte(b.String()), c.Marshal(), nil
}
