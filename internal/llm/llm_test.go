package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func fake(t *testing.T, replies ...string) *Client {
	i := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			ResponseFormat map[string]string `json:"response_format"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		if r.Header.Get("Authorization") != "Bearer k" {
			t.Errorf("missing API key")
		}
		reply := replies[min(i, len(replies)-1)]
		i++
		json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"role": "assistant", "content": reply}}}})
	}))
	t.Cleanup(srv.Close)
	return New(srv.URL, "k", "m")
}

func TestJSONRetriesInvalidReply(t *testing.T) {
	c := fake(t, "sorry, here you go: not json", "```json\n{\"price\": 20, \"currency\": \"INR\"}\n```")
	v, err := c.JSON(context.Background(), "extract", "page", json.RawMessage(`{"type":"object","required":["price"]}`))
	if err != nil {
		t.Fatal(err)
	}
	if v.(map[string]any)["price"] != float64(20) {
		t.Fatalf("got %v", v)
	}
}

func TestJSONMissingRequiredField(t *testing.T) {
	c := fake(t, `{"currency":"INR"}`, `{"currency":"INR"}`)
	if _, err := c.JSON(context.Background(), "x", "y", json.RawMessage(`{"type":"object","required":["price"]}`)); err == nil {
		t.Fatal("accepted JSON missing a required field")
	}
}

func TestNotConfigured(t *testing.T) {
	var c *Client
	if _, err := c.Chat(context.Background(), nil, false); err != ErrNotConfigured {
		t.Fatalf("got %v", err)
	}
}

func TestAnthropicMessagesAPI(t *testing.T) {
	var gotPath, gotKey, gotVersion string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotKey, gotVersion = r.URL.Path, r.Header.Get("x-api-key"), r.Header.Get("anthropic-version")
		if r.URL.Path == "/v1/models" {
			w.Write([]byte(`{"data":[{"id":"claude-haiku-5-5"},{"id":"claude-sonnet-5-5"}]}`))
			return
		}
		json.NewDecoder(r.Body).Decode(&gotBody)
		w.Write([]byte("{\"content\":[{\"type\":\"text\",\"text\":\"```json\\n{\\\"ok\\\":true}\\n```\"}]}"))
	}))
	defer srv.Close()
	c := NewFor("anthropic", srv.URL+"/v1", "sk-ant-x", "claude-haiku-5-5")
	v, err := c.JSON(context.Background(), "Extract.", "page", nil)
	if err != nil || v.(map[string]any)["ok"] != true {
		t.Fatalf("JSON: %v %v", v, err)
	}
	if gotPath != "/v1/messages" || gotKey != "sk-ant-x" || gotVersion == "" {
		t.Fatalf("request: %s key=%q version=%q", gotPath, gotKey, gotVersion)
	}
	if !strings.Contains(gotBody["system"].(string), "Extract.") || gotBody["max_tokens"] == nil {
		t.Fatalf("body: %v", gotBody)
	}
	for _, m := range gotBody["messages"].([]any) {
		if m.(map[string]any)["role"] == "system" {
			t.Fatal("system message must not be in messages")
		}
	}
	ids, err := c.Models(context.Background())
	if err != nil || len(ids) != 2 || gotKey != "sk-ant-x" {
		t.Fatalf("models: %v %v", ids, err)
	}
}
