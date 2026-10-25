package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"hivecrawl/internal/cache"
	"hivecrawl/internal/llm"
	"hivecrawl/internal/node"
	"hivecrawl/internal/scrape"
	"hivecrawl/internal/searx"
)

// agentSite is the test site's URL, substituted for "SITE" in agent scripts.
var agentSite atomic.Value

// fakeLLM answers by looking at the system prompt; the agent gets a script.
func fakeLLM(t *testing.T, agentScript []string) *llm.Client {
	var mu sync.Mutex
	step := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []llm.Message `json:"messages"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		sys := body.Messages[0].Content
		var reply string
		switch {
		case strings.Contains(sys, "Summarize"):
			reply = "Kochi Metro trains run every 7 minutes."
		case strings.Contains(sys, "research agent"):
			mu.Lock()
			reply = strings.ReplaceAll(agentScript[min(step, len(agentScript)-1)], "SITE", agentSite.Load().(string))
			step++
			mu.Unlock()
		default: // extraction
			reply = `{"fare": 20, "pages_seen": ` + fmt.Sprint(strings.Count(body.Messages[1].Content, "=== Page:")) + `}`
		}
		json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"role": "assistant", "content": reply}}}})
	}))
	t.Cleanup(srv.Close)
	return llm.New(srv.URL, "", "fake")
}

func llmServer(t *testing.T, c *llm.Client) (*httptest.Server, *httptest.Server) {
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprintf(w, "<html><head><title>Fares %s</title></head><body><main><p>The fare is 20 rupees. Trains every 7 minutes.</p></main></body></html>", r.URL.Path)
	}))
	t.Cleanup(site.Close)
	agentSite.Store(site.URL)
	sx := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"results":[{"url":%q,"title":"Fares","content":"fare info","engines":["google cse"]}]}`, site.URL+"/fares")
	}))
	t.Cleanup(sx.Close)
	n := node.New("test", cache.New(time.Minute, 10), searx.New(sx.URL, http.DefaultClient), 1)
	mux := http.NewServeMux()
	(&Server{Node: n, Scraper: &scrape.Scraper{Direct: scrape.StdClient(&http.Client{})}, LLM: c, Started: time.Now()}).Register(mux)
	api := httptest.NewServer(mux)
	t.Cleanup(api.Close)
	return api, site
}

func TestJSONAndSummaryFormats(t *testing.T) {
	api, site := llmServer(t, fakeLLM(t, nil))
	m := call(t, "POST", api.URL+"/v2/scrape", fmt.Sprintf(`{"url":%q,"formats":["markdown","summary",{"type":"json","schema":{"type":"object","properties":{"fare":{"type":"number"}},"required":["fare"]}}]}`, site.URL+"/fares"))
	d := m["data"].(map[string]any)
	if d["summary"] != "Kochi Metro trains run every 7 minutes." || d["json"].(map[string]any)["fare"] != float64(20) {
		t.Fatalf("formats: summary=%v json=%v warning=%v", d["summary"], d["json"], d["warning"])
	}
}

func TestExtractJob(t *testing.T) {
	api, site := llmServer(t, fakeLLM(t, nil))
	start := call(t, "POST", api.URL+"/v2/extract", fmt.Sprintf(`{"urls":[%q,%q],"prompt":"fare","showSources":true,"enableWebSearch":true}`, site.URL+"/a", site.URL+"/b"))
	id := start["id"].(string)
	var st map[string]any
	for range 100 {
		if st = call(t, "GET", api.URL+"/v2/extract/"+id, ""); st["status"] != "processing" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	data := st["data"].(map[string]any)
	// two given URLs + one from web search
	if st["status"] != "completed" || data["fare"] != float64(20) || data["pages_seen"] != float64(3) || len(st["sources"].([]any)) != 3 {
		t.Fatalf("extract: %v", st)
	}
}

func TestAgentJob(t *testing.T) {
	var siteURL string
	c := fakeLLM(t, []string{
		`{"action":"search","query":"kochi metro fare"}`,
		`{"action":"scrape","url":"SITE/fares"}`,
		`{"action":"finish","data":{"fare":20,"sources":["SITE/fares"]}}`,
	})
	api, site := llmServer(t, c)
	siteURL = site.URL
	_ = siteURL
	start := call(t, "POST", api.URL+"/v2/agent", `{"prompt":"What is the Kochi metro fare?"}`)
	id := start["id"].(string)
	var st map[string]any
	for range 100 {
		if st = call(t, "GET", api.URL+"/v2/agent/"+id, ""); st["status"] != "processing" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if st["status"] != "completed" || st["data"].(map[string]any)["fare"] != float64(20) {
		t.Fatalf("agent: %v", st)
	}
}

func TestLLMNotConfigured(t *testing.T) {
	api, site := llmServer(t, nil)
	if m := call(t, "POST", api.URL+"/v2/extract", `{"urls":["https://a.com"],"prompt":"x"}`); m["_status"].(float64) != 400 || !strings.Contains(m["error"].(string), "HC_LLM") {
		t.Fatalf("extract without LLM: %v", m)
	}
	m := call(t, "POST", api.URL+"/v2/scrape", fmt.Sprintf(`{"url":%q,"formats":["summary"]}`, site.URL))
	if !strings.Contains(fmt.Sprint(m["data"].(map[string]any)["warning"]), "HC_LLM") {
		t.Fatalf("summary without LLM should warn: %v", m)
	}
}

func TestAgentMustResearchAndCiteOnlySeenPages(t *testing.T) {
	c := fakeLLM(t, []string{
		`{"action":"finish","data":{"answer":"from memory","sources":["https://made-up.example/"]}}`,
		`{"action":"scrape","url":"SITE"}`,
		`{"action":"finish","data":{"answer":"20 rupees","sources":["SITE","https://made-up.example/"]}}`,
	})
	api, site := llmServer(t, c)
	_ = site
	start := call(t, "POST", api.URL+"/v2/agent", `{"prompt":"fare?"}`)
	var st map[string]any
	for range 100 {
		if st = call(t, "GET", api.URL+"/v2/agent/"+start["id"].(string), ""); st["status"] != "processing" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	d := st["data"].(map[string]any)
	if d["answer"] != "20 rupees" {
		t.Fatalf("agent answered from memory: %v", st)
	}
	for _, s := range d["sources"].([]any) {
		if strings.Contains(s.(string), "made-up") {
			t.Fatalf("unseen source kept: %v", d["sources"])
		}
	}
}
