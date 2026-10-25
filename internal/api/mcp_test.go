package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"hivecrawl/internal/cache"
	"hivecrawl/internal/node"
	"hivecrawl/internal/scrape"
	"hivecrawl/internal/searx"
)

type bearer struct{ key string }

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.key)
	return http.DefaultTransport.RoundTrip(r)
}

func mcpServer(t *testing.T) (*httptest.Server, string) {
	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<html><head><title>Onam 2026</title></head><body><nav>menu</nav><main><h1>Onam</h1><p>Thiruvonam is on <b>26 August</b>.</p></main></body></html>`))
	}))
	t.Cleanup(page.Close)
	sx := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"results":[{"url":"` + page.URL + `/onam","title":"Onam date","content":"When is Onam","engines":["google cse"]}]}`))
	}))
	t.Cleanup(sx.Close)
	n := node.New("test", cache.New(time.Minute, 10), searx.New(sx.URL, http.DefaultClient), 1)
	mux := http.NewServeMux()
	// Plain client so the test page on 127.0.0.1 is reachable.
	(&Server{Node: n, APIKeys: []string{"k1"}, Scraper: &scrape.Scraper{Direct: scrape.StdClient(&http.Client{})}, Started: time.Now()}).Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, page.URL
}

func connect(t *testing.T, url, key string) (*mcp.ClientSession, error) {
	c := mcp.NewClient(&mcp.Implementation{Name: "test-agent", Version: "1"}, nil)
	return c.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint: url + "/mcp", HTTPClient: &http.Client{Transport: bearer{key}},
	}, nil)
}

func TestMCPToolsWork(t *testing.T) {
	srv, pageURL := mcpServer(t)
	s, err := connect(t, srv.URL, "k1")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()

	tools, err := s.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tl := range tools.Tools {
		names = append(names, tl.Name)
	}
	sort.Strings(names)
	if strings.Join(names, ",") != "hivecrawl_agent,hivecrawl_crawl,hivecrawl_crawl_status,hivecrawl_extract,hivecrawl_map,hivecrawl_scrape,hivecrawl_search" {
		t.Fatalf("tools = %v", names)
	}

	res, err := s.CallTool(ctx, &mcp.CallToolParams{Name: "hivecrawl_search", Arguments: map[string]any{"query": "onam date", "scrape": true}})
	if err != nil || res.IsError {
		t.Fatalf("search: %v %+v", err, res)
	}
	text := res.Content[0].(*mcp.TextContent).Text
	if !strings.Contains(text, "Onam date") || !strings.Contains(text, "**26 August**") || strings.Contains(text, "menu") {
		t.Fatalf("search text (with scraped content) wrong:\n%s", text)
	}

	res, err = s.CallTool(ctx, &mcp.CallToolParams{Name: "hivecrawl_scrape", Arguments: map[string]any{"url": pageURL + "/onam"}})
	if err != nil || res.IsError {
		t.Fatalf("scrape: %v %+v", err, res)
	}
	text = res.Content[0].(*mcp.TextContent).Text
	if !strings.HasPrefix(text, "# Onam 2026") || !strings.Contains(text, "# Onam\n") {
		t.Fatalf("scrape text wrong:\n%s", text)
	}

	// Bad input comes back as a tool error, not a crash.
	res, err = s.CallTool(ctx, &mcp.CallToolParams{Name: "hivecrawl_scrape", Arguments: map[string]any{"url": "file:///etc/passwd"}})
	if err != nil || !res.IsError {
		t.Fatalf("want tool error for file:// URL, got err=%v res=%+v", err, res)
	}
}

func TestMCPNeedsAPIKey(t *testing.T) {
	srv, _ := mcpServer(t)
	if s, err := connect(t, srv.URL, "wrong"); err == nil {
		s.Close()
		t.Fatal("connected to MCP with a wrong API key")
	}
}

func TestMCPMapAndCrawl(t *testing.T) {
	srv, pageURL := mcpServer(t)
	s, err := connect(t, srv.URL, "k1")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	res, err := s.CallTool(ctx, &mcp.CallToolParams{Name: "hivecrawl_map", Arguments: map[string]any{"url": pageURL + "/onam"}})
	if err != nil || res.IsError || !strings.Contains(res.Content[0].(*mcp.TextContent).Text, "/onam") {
		t.Fatalf("map: %v %+v", err, res)
	}
	res, err = s.CallTool(ctx, &mcp.CallToolParams{Name: "hivecrawl_crawl", Arguments: map[string]any{"url": pageURL + "/onam", "limit": 1}})
	if err != nil || res.IsError {
		t.Fatalf("crawl: %v %+v", err, res)
	}
	id := res.StructuredContent.(map[string]any)["id"].(string)
	var text string
	for range 100 {
		res, _ = s.CallTool(ctx, &mcp.CallToolParams{Name: "hivecrawl_crawl_status", Arguments: map[string]any{"id": id}})
		text = res.Content[0].(*mcp.TextContent).Text
		if strings.Contains(text, "completed") {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(text, "completed, 1 of 1") || !strings.Contains(text, "26 August") {
		t.Fatalf("crawl status: %s", text)
	}
	res, _ = s.CallTool(ctx, &mcp.CallToolParams{Name: "hivecrawl_extract", Arguments: map[string]any{"urls": []string{pageURL}, "prompt": "date"}})
	if !res.IsError || !strings.Contains(res.Content[0].(*mcp.TextContent).Text, "HC_LLM") {
		t.Fatalf("extract without LLM must be a clear tool error: %+v", res)
	}
}
