package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"hivecrawl/internal/cache"
	"hivecrawl/internal/node"
	"hivecrawl/internal/scrape"
	"hivecrawl/internal/searx"
	"hivecrawl/internal/settings"
)

func fakeProvider(t *testing.T, wantKey string) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+wantKey {
			http.Error(w, `{"error":"bad key"}`, http.StatusUnauthorized)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/models") {
			w.Write([]byte(`{"data":[{"id":"model-a"},{"id":"model-b"}]}`))
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"role": "assistant", "content": "OK"}}}})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func settingsServer(t *testing.T, dir string) (*httptest.Server, *Server) {
	n := node.New("test", cache.New(time.Minute, 10), searx.New("http://127.0.0.1:1", http.DefaultClient), 1)
	s := &Server{Node: n, Scraper: &scrape.Scraper{Direct: scrape.StdClient(&http.Client{})}, DataDir: dir, Started: time.Now()}
	mux := http.NewServeMux()
	s.Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, s
}

func TestLLMSettingsFlow(t *testing.T) {
	prov := fakeProvider(t, "sk-secret-1234")
	dir := t.TempDir()
	api, s := settingsServer(t, dir)

	if m := call(t, "GET", api.URL+"/v1/settings", ""); m["llm"].(map[string]any)["configured"] != false {
		t.Fatalf("starts unconfigured: %v", m)
	}
	body := `{"provider":"custom","baseUrl":"` + prov.URL + `","model":"model-a","apiKey":"sk-secret-1234"}`
	if m := call(t, "POST", api.URL+"/v1/settings/llm/models", body); m["success"] != true || len(m["models"].([]any)) != 2 {
		t.Fatalf("models: %v", m)
	}
	if m := call(t, "POST", api.URL+"/v1/settings/llm/test", body); m["success"] != true || m["reply"] != "OK" {
		t.Fatalf("test: %v", m)
	}
	m := call(t, "PUT", api.URL+"/v1/settings/llm", body)
	l := m["llm"].(map[string]any)
	if l["configured"] != true || l["keySet"] != true || l["keyHint"] != "sk-…1234" {
		t.Fatalf("save: %v", m)
	}
	raw, _ := json.Marshal(m)
	if strings.Contains(string(raw), "secret") {
		t.Fatal("API key leaked in the settings response")
	}
	if s.llm() == nil || s.llm().Model != "model-a" {
		t.Fatal("new LLM not applied immediately")
	}
	// Change the model without retyping the key: the saved key is kept.
	call(t, "PUT", api.URL+"/v1/settings/llm", `{"provider":"custom","baseUrl":"`+prov.URL+`","model":"model-b"}`)
	if m := call(t, "POST", api.URL+"/v1/settings/llm/test", `{"baseUrl":"`+prov.URL+`","model":"model-b"}`); m["success"] != true {
		t.Fatalf("saved key not reused: %v", m)
	}
	// Saved to disk, owner-only, and loaded again by a new server.
	if fi, err := os.Stat(filepath.Join(dir, "settings.json")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("settings file: %v %v", err, fi)
	}
	saved, _ := settings.Load(dir)
	if saved.LLM.Model != "model-b" || saved.LLM.APIKey != "sk-secret-1234" {
		t.Fatalf("saved: %+v", saved.LLM)
	}
	// Remove: LLM features turn off.
	if m := call(t, "DELETE", api.URL+"/v1/settings/llm", ""); m["llm"].(map[string]any)["configured"] != false || s.llm() != nil {
		t.Fatalf("delete: %v", m)
	}
}

func TestLLMSettingsValidation(t *testing.T) {
	api, _ := settingsServer(t, t.TempDir())
	for _, body := range []string{`{"baseUrl":"ftp://x","model":"m"}`, `{"baseUrl":"https://a.com","model":""}`} {
		if m := call(t, "PUT", api.URL+"/v1/settings/llm", body); m["_status"].(float64) != 400 {
			t.Errorf("%s: want 400, got %v", body, m)
		}
	}
}
