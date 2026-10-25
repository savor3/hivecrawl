package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"hivecrawl/internal/cache"
	"hivecrawl/internal/node"
	"hivecrawl/internal/searx"
)

// server returns an API backed by a fake SearXNG that echoes what it got.
var fixtureMu sync.Mutex

func server(t *testing.T, gotQuery *string, gotRange *string) *httptest.Server {
	sx := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Web and news searches run in parallel; record the web one only.
		if r.URL.Query().Get("categories") == "" {
			fixtureMu.Lock()
			*gotQuery, *gotRange = r.URL.Query().Get("q"), r.URL.Query().Get("time_range")
			fixtureMu.Unlock()
		}
		var res []map[string]any
		for i, u := range []string{"https://go.dev/a", "https://www.github.com/b", "https://sub.go.dev/c", "ftp://bad/x", "https://reddit.com/d", "https://go.dev/e", "https://go.dev/f", "https://go.dev/g", "https://go.dev/h"} {
			res = append(res, map[string]any{"url": u, "title": fmt.Sprint("t", i), "content": "c", "engines": []string{"google cse"}})
		}
		json.NewEncoder(w).Encode(map[string]any{"results": res})
	}))
	t.Cleanup(sx.Close)
	n := node.New("test", cache.New(time.Minute, 10), searx.New(sx.URL, http.DefaultClient), 1)
	mux := http.NewServeMux()
	(&Server{Node: n, APIKeys: []string{"fc-test"}, Started: time.Now()}).Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func post(t *testing.T, url, key, body string) (int, map[string]any, http.Header) {
	t.Helper()
	req, _ := http.NewRequest("POST", url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var m map[string]any
	json.NewDecoder(resp.Body).Decode(&m)
	return resp.StatusCode, m, resp.Header
}

func keys(m map[string]any) string {
	var k []string
	for x := range m {
		k = append(k, x)
	}
	sort.Strings(k)
	return strings.Join(k, ",")
}

func TestV2ResponseIsFirecrawlShape(t *testing.T) {
	var q, tr string
	srv := server(t, &q, &tr)
	code, m, h := post(t, srv.URL+"/v2/search", "fc-test", `{"query":"golang","limit":3}`)
	if code != 200 {
		t.Fatalf("status %d: %v", code, m)
	}
	if got := keys(m); got != "creditsUsed,data,id,success" {
		t.Fatalf("top-level keys = %s, want Firecrawl's creditsUsed,data,id,success", got)
	}
	data := m["data"].(map[string]any)
	if got := keys(data); got != "web" {
		t.Fatalf("data keys = %s, want web", got)
	}
	web := data["web"].([]any)
	if len(web) != 3 {
		t.Fatalf("limit not applied: %d results", len(web))
	}
	for i, x := range web {
		r := x.(map[string]any)
		if got := keys(r); got != "description,position,title,url" {
			t.Fatalf("result keys = %s, want description,position,title,url", got)
		}
		if int(r["position"].(float64)) != i+1 {
			t.Fatalf("position %v at index %d", r["position"], i)
		}
	}
	if h.Get("X-HC-Source") != "local" || h.Get("X-HC-Node") != "test" {
		t.Fatalf("HiveCrawl headers missing: %v", h)
	}
}

func TestV1ResponseIsFirecrawlShape(t *testing.T) {
	var q, tr string
	srv := server(t, &q, &tr)
	_, m, _ := post(t, srv.URL+"/v1/search", "fc-test", `{"query":"golang","limit":2}`)
	if got := keys(m); got != "data,success" {
		t.Fatalf("v1 keys = %s", got)
	}
	if n := len(m["data"].([]any)); n != 2 {
		t.Fatalf("v1 data has %d results", n)
	}
}

func TestErrorsAreFirecrawlShape(t *testing.T) {
	var q, tr string
	srv := server(t, &q, &tr)
	for _, c := range []struct {
		key, body string
		code      int
	}{
		{"", `{"query":"x"}`, 401},
		{"wrong", `{"query":"x"}`, 401},
		{"fc-test", `{"query":""}`, 400},
		{"fc-test", `{"query":"x","limit":500}`, 400},
		{"fc-test", `{"query":"x","sources":["videos"]}`, 400},
		{"fc-test", `{"query":"x","tbs":"bogus"}`, 400},
		{"fc-test", `not json`, 400},
	} {
		code, m, _ := post(t, srv.URL+"/v2/search", c.key, c.body)
		if code != c.code || keys(m) != "error,success" || m["success"] != false {
			t.Errorf("%s with key %q: got %d %v, want %d {success:false,error}", c.body, c.key, code, m, c.code)
		}
	}
}

func TestDomainFiltersTbsAndInvalidURLs(t *testing.T) {
	var q, tr string
	srv := server(t, &q, &tr)
	_, m, _ := post(t, srv.URL+"/v2/search", "fc-test", `{"query":"golang","includeDomains":["go.dev"],"tbs":"qdr:w","ignoreInvalidURLs":true}`)
	if q != "golang (site:go.dev)" || tr != "week" {
		t.Fatalf("searxng got q=%q time_range=%q", q, tr)
	}
	for _, x := range m["data"].(map[string]any)["web"].([]any) {
		u := x.(map[string]any)["url"].(string)
		if !strings.Contains(u, "go.dev") {
			t.Fatalf("includeDomains let through %s", u)
		}
	}
	_, m, _ = post(t, srv.URL+"/v2/search", "fc-test", `{"query":"golang2","excludeDomains":["go.dev"],"ignoreInvalidURLs":true}`)
	if q != "golang2 -site:go.dev" {
		t.Fatalf("exclude query = %q", q)
	}
	for _, x := range m["data"].(map[string]any)["web"].([]any) {
		u := x.(map[string]any)["url"].(string)
		if strings.Contains(u, "go.dev") || strings.HasPrefix(u, "ftp:") {
			t.Fatalf("filter let through %s", u)
		}
	}
	_, m, _ = post(t, srv.URL+"/v2/search", "fc-test", `{"query":"golang3","sources":["web","news"]}`)
	data := m["data"].(map[string]any)
	if keys(data) != "news,web" {
		t.Fatalf("want news and web groups, got %v", keys(data))
	}
	n := data["news"].([]any)[0].(map[string]any)
	if got := keys(n); got != "position,snippet,title,url" {
		t.Fatalf("news result keys = %s, want Firecrawl's (date/imageUrl only when known)", got)
	}
}

func TestParseSourcesForms(t *testing.T) {
	for raw, want := range map[string]string{
		``: "[web]", `["news"]`: "[news]", `[{"type":"news"}]`: "[news]", `[{"type":"web"},{"type":"images"}]`: "[web images]",
	} {
		got, err := parseSources(json.RawMessage(raw))
		if err != nil || fmt.Sprint(got) != want {
			t.Errorf("%s: got %v %v, want %s", raw, got, err, want)
		}
	}
	if _, err := parseSources(json.RawMessage(`[{"type":"video"}]`)); err == nil {
		t.Error("unknown source accepted")
	}
}
