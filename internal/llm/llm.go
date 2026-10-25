// Package llm is a minimal client for any OpenAI-compatible chat API
// (OpenAI, DeepSeek, OpenRouter, Gemini, Ollama, …) and for Anthropic's
// Messages API. It only relies on plain chat and JSON output.
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type Client struct {
	// Anthropic selects the Messages API (/v1/messages, x-api-key) instead
	// of OpenAI-style /chat/completions.
	Anthropic bool
	BaseURL   string // e.g. https://api.openai.com/v1
	APIKey    string
	Model     string
	HTTP      *http.Client
}

func New(baseURL, apiKey, model string) *Client {
	return &Client{BaseURL: strings.TrimSuffix(baseURL, "/"), APIKey: apiKey, Model: model, HTTP: &http.Client{Timeout: 3 * time.Minute}}
}

// NewFor is New for a named provider ("anthropic" uses its own API).
func NewFor(provider, baseURL, apiKey, model string) *Client {
	c := New(baseURL, apiKey, model)
	c.Anthropic = provider == "anthropic"
	return c
}

func (c *Client) authorize(req *http.Request) {
	if c.APIKey == "" {
		return
	}
	if c.Anthropic {
		req.Header.Set("x-api-key", c.APIKey)
		req.Header.Set("anthropic-version", "2023-06-01")
		return
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
}

type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

var ErrNotConfigured = errors.New("no LLM configured: set HC_LLM_BASE_URL, HC_LLM_MODEL and (for hosted APIs) HC_LLM_API_KEY")

// Chat returns the assistant's reply. With jsonMode the model must answer
// with a single JSON object.
func (c *Client) Chat(ctx context.Context, msgs []Message, jsonMode bool) (string, error) {
	if c == nil || c.Model == "" || c.BaseURL == "" {
		return "", ErrNotConfigured
	}
	if c.Anthropic {
		return c.chatAnthropic(ctx, msgs)
	}
	body := map[string]any{"model": c.Model, "messages": msgs, "temperature": 0.1}
	if jsonMode {
		body["response_format"] = map[string]string{"type": "json_object"}
	}
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/chat/completions", bytes.NewReader(b))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	c.authorize(req)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("llm: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("llm: status %d: %.300s", resp.StatusCode, raw)
	}
	var out struct {
		Choices []struct {
			Message Message `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || len(out.Choices) == 0 {
		return "", fmt.Errorf("llm: unexpected response: %.300s", raw)
	}
	return out.Choices[0].Message.Content, nil
}

// JSON asks for a JSON object (optionally matching schema) and decodes it,
// retrying once if the reply is not valid JSON.
func (c *Client) JSON(ctx context.Context, system, user string, schema json.RawMessage) (any, error) {
	sys := system + "\n\nAnswer with one JSON object only, no prose."
	if len(schema) > 0 && string(schema) != "null" {
		sys += " The JSON must conform to this JSON Schema (use null for anything the content does not contain):\n" + string(schema)
	}
	msgs := []Message{{Role: "system", Content: sys}, {Role: "user", Content: user}}
	var lastErr error
	for range 2 {
		reply, err := c.Chat(ctx, msgs, true)
		if err != nil {
			return nil, err
		}
		v, err := parseJSON(reply)
		if err == nil {
			if verr := check(schema, v); verr == nil {
				return v, nil
			} else {
				lastErr = verr
			}
		} else {
			lastErr = err
		}
		msgs = append(msgs, Message{Role: "assistant", Content: reply},
			Message{Role: "user", Content: "That was not valid: " + lastErr.Error() + ". Reply again with only the corrected JSON object."})
	}
	return nil, fmt.Errorf("llm returned invalid JSON: %w", lastErr)
}

// parseJSON tolerates code fences around the object.
func parseJSON(s string) (any, error) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "```json")
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimSuffix(s, "```")
	var v any
	if err := json.Unmarshal([]byte(strings.TrimSpace(s)), &v); err != nil {
		if i, j := strings.Index(s, "{"), strings.LastIndex(s, "}"); i >= 0 && j > i {
			if err2 := json.Unmarshal([]byte(s[i:j+1]), &v); err2 == nil {
				return v, nil
			}
		}
		return nil, err
	}
	return v, nil
}

// check does a light schema check: top-level type and required keys.
func check(schema json.RawMessage, v any) error {
	if len(schema) == 0 || string(schema) == "null" {
		return nil
	}
	var s struct {
		Type     string   `json:"type"`
		Required []string `json:"required"`
	}
	if json.Unmarshal(schema, &s) != nil {
		return nil
	}
	obj, isObj := v.(map[string]any)
	if (s.Type == "object" || len(s.Required) > 0) && !isObj {
		return errors.New("expected a JSON object")
	}
	for _, k := range s.Required {
		if _, ok := obj[k]; !ok {
			return fmt.Errorf("missing required field %q", k)
		}
	}
	return nil
}

// Truncate keeps text within n runes (pages can be huge).
func Truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "\n…(truncated)"
}

// Models lists the provider's model IDs (GET /models, OpenAI-compatible).
func (c *Client) Models(ctx context.Context) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/models?limit=1000", nil)
	if err != nil {
		return nil, err
	}
	c.authorize(req)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d: %.200s", resp.StatusCode, raw)
	}
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("unexpected response: %.200s", raw)
	}
	ids := make([]string, 0, len(out.Data))
	for _, m := range out.Data {
		ids = append(ids, m.ID)
	}
	return ids, nil
}

// chatAnthropic calls the Messages API. System messages go in "system";
// JSON output relies on the prompt (parseJSON tolerates code fences).
func (c *Client) chatAnthropic(ctx context.Context, msgs []Message) (string, error) {
	var system []string
	var turns []Message
	for _, m := range msgs {
		if m.Role == "system" {
			system = append(system, m.Content)
		} else {
			turns = append(turns, m)
		}
	}
	body := map[string]any{"model": c.Model, "max_tokens": 8192, "messages": turns, "temperature": 0.1}
	if len(system) > 0 {
		body["system"] = strings.Join(system, "\n\n")
	}
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/messages", bytes.NewReader(b))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	c.authorize(req)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("llm: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("llm: status %d: %.300s", resp.StatusCode, raw)
	}
	var out struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("llm: unexpected response: %.300s", raw)
	}
	var text strings.Builder
	for _, p := range out.Content {
		if p.Type == "text" {
			text.WriteString(p.Text)
		}
	}
	if text.Len() == 0 {
		return "", fmt.Errorf("llm: empty reply: %.300s", raw)
	}
	return text.String(), nil
}
