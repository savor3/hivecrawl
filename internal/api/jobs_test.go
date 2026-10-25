package api

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"hivecrawl/internal/cache"
	"hivecrawl/internal/node"
	"hivecrawl/internal/scrape"
	"hivecrawl/internal/searx"
)

// testSite: /docs links to 12 child pages; robots.txt disallows /docs/secret.
func testSite(t *testing.T) *httptest.Server {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/robots.txt":
			fmt.Fprint(w, "User-agent: *\nDisallow: /docs/secret\n")
		case r.URL.Path == "/docs":
			w.Header().Set("Content-Type", "text/html")
			var b strings.Builder
			b.WriteString("<html><head><title>Docs</title></head><body><main>")
			for i := range 12 {
				fmt.Fprintf(&b, `<a href="/docs/p%d">page %d</a> `, i, i)
			}
			b.WriteString(`<a href="/docs/secret">secret</a><a href="/docs/missing">missing</a></main></body></html>`)
			w.Write([]byte(b.String()))
		case strings.HasPrefix(r.URL.Path, "/docs/p"):
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprintf(w, "<html><head><title>%s</title></head><body><main><p>content of %s</p></main></body></html>", r.URL.Path, r.URL.Path)
		case r.URL.Path == "/slow":
			time.Sleep(2 * time.Second)
			w.Write([]byte("<html><body>slow</body></html>"))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func jobServer(t *testing.T, secret string) *httptest.Server {
	sx := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"results":[]}`))
	}))
	t.Cleanup(sx.Close)
	n := node.New("test", cache.New(time.Minute, 10), searx.New(sx.URL, http.DefaultClient), 1)
	mux := http.NewServeMux()
	(&Server{Node: n, Scraper: &scrape.Scraper{Direct: scrape.StdClient(&http.Client{})}, WebhookSecret: secret, Started: time.Now()}).Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func call(t *testing.T, method, url, body string) map[string]any {
	t.Helper()
	req, _ := http.NewRequest(method, url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var m map[string]any
	json.NewDecoder(resp.Body).Decode(&m)
	m["_status"] = float64(resp.StatusCode)
	return m
}

func waitDone(t *testing.T, url string) map[string]any {
	t.Helper()
	for range 200 {
		m := call(t, "GET", url, "")
		if m["status"] != "scraping" {
			return m
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("job did not finish")
	return nil
}

func TestCrawlJobFlow(t *testing.T) {
	site, api := testSite(t), jobServer(t, "")
	start := call(t, "POST", api.URL+"/v2/crawl", fmt.Sprintf(`{"url":%q,"limit":50,"scrapeOptions":{"formats":["markdown"]}}`, site.URL+"/docs"))
	if start["success"] != true || start["id"] == "" || !strings.HasSuffix(start["url"].(string), "/v2/crawl/"+start["id"].(string)) {
		t.Fatalf("start: %v", start)
	}
	st := waitDone(t, start["url"].(string))
	// /docs + 12 pages + /docs/missing (a 404 is returned with its status, as Firecrawl does).
	if st["status"] != "completed" || st["completed"].(float64) != 14 || len(st["data"].([]any)) != 14 {
		t.Fatalf("status: status=%v completed=%v data=%d", st["status"], st["completed"], len(st["data"].([]any)))
	}
	found404 := false
	for _, d := range st["data"].([]any) {
		m := d.(map[string]any)["metadata"].(map[string]any)
		if strings.HasSuffix(m["url"].(string), "/docs/missing") {
			found404 = m["statusCode"].(float64) == 404 && m["error"] == "Not Found"
		}
	}
	if !found404 {
		t.Fatal("404 page not reported with statusCode 404")
	}
	doc := st["data"].([]any)[0].(map[string]any)
	if doc["markdown"] == nil || doc["metadata"].(map[string]any)["sourceURL"] == nil {
		t.Fatalf("doc shape: %v", doc)
	}
	if doc["links"] != nil {
		t.Fatal("links leaked into docs though not requested")
	}
	if !strings.Contains(fmt.Sprint(st["warning"]), "robots.txt") {
		t.Fatalf("want robots warning, got %v", st["warning"])
	}
	errs := call(t, "GET", start["url"].(string)+"/errors", "")
	if fmt.Sprint(errs["robotsBlocked"]) != fmt.Sprintf("[%s/docs/secret]", site.URL) {
		t.Fatalf("robotsBlocked = %v", errs["robotsBlocked"])
	}
	// Pagination: 5 per page.
	page := call(t, "GET", start["url"].(string)+"?limit=5", "")
	if len(page["data"].([]any)) != 5 || !strings.Contains(page["next"].(string), "skip=5&limit=5") {
		t.Fatalf("pagination: %d docs, next=%v", len(page["data"].([]any)), page["next"])
	}
	last := call(t, "GET", start["url"].(string)+"?skip=10&limit=5", "")
	if len(last["data"].([]any)) != 4 || last["next"] != nil {
		t.Fatalf("last page: %d docs, next=%v", len(last["data"].([]any)), last["next"])
	}
}

func TestCrawlCancelAndOngoing(t *testing.T) {
	site, api := testSite(t), jobServer(t, "")
	start := call(t, "POST", api.URL+"/v2/crawl", fmt.Sprintf(`{"url":%q,"limit":50,"delay":1}`, site.URL+"/docs"))
	ongoing := call(t, "GET", api.URL+"/v2/crawl/ongoing", "")
	if len(ongoing["crawls"].([]any)) != 1 {
		t.Fatalf("ongoing: %v", ongoing)
	}
	c := call(t, "DELETE", start["url"].(string), "")
	if c["status"] != "cancelled" {
		t.Fatalf("cancel: %v", c)
	}
	if st := call(t, "GET", start["url"].(string), ""); st["status"] != "cancelled" {
		t.Fatalf("status after cancel: %v", st["status"])
	}
}

func TestCrawlValidation(t *testing.T) {
	api := jobServer(t, "")
	for _, body := range []string{`{"url":"ftp://x"}`, `{"url":"https://a.com","includePaths":["("]}`, `{"url":"https://a.com","sitemap":"maybe"}`, `{"url":"https://a.com","delay":120}`} {
		if m := call(t, "POST", api.URL+"/v2/crawl", body); m["_status"].(float64) != 400 || m["success"] != false {
			t.Errorf("%s: want 400, got %v", body, m)
		}
	}
	if m := call(t, "GET", api.URL+"/v2/crawl/nope", ""); m["_status"].(float64) != 404 {
		t.Errorf("unknown job: %v", m)
	}
}

func TestBatchScrapeWithWebhook(t *testing.T) {
	site := testSite(t)
	var mu sync.Mutex
	var events []string
	var sigOK = true
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		m := hmac.New(sha256.New, []byte("s3cret"))
		m.Write(body)
		var p map[string]any
		json.Unmarshal(body, &p)
		mu.Lock()
		events = append(events, p["type"].(string))
		if r.Header.Get("X-Firecrawl-Signature") != "sha256="+hex.EncodeToString(m.Sum(nil)) || r.Header.Get("X-Custom") != "1" {
			sigOK = false
		}
		mu.Unlock()
	}))
	defer hook.Close()
	api := jobServer(t, "s3cret")
	body := fmt.Sprintf(`{"urls":[%q,%q,"not a url"],"formats":["markdown"],"webhook":{"url":%q,"headers":{"X-Custom":"1"}}}`, site.URL+"/docs/p1", site.URL+"/docs/missing", hook.URL)
	start := call(t, "POST", api.URL+"/v2/batch/scrape", body)
	if fmt.Sprint(start["invalidURLs"]) != "[not a url]" {
		t.Fatalf("invalidURLs: %v", start)
	}
	st := waitDone(t, start["url"].(string))
	if st["status"] != "completed" || st["total"].(float64) != 2 || len(st["data"].([]any)) != 2 {
		t.Fatalf("batch status: %v total=%v data=%d", st["status"], st["total"], len(st["data"].([]any)))
	}
	time.Sleep(200 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	got := strings.Join(events, ",")
	for _, want := range []string{"batch_scrape.started", "batch_scrape.page", "batch_scrape.completed"} {
		if !strings.Contains(got, want) {
			t.Errorf("webhook events %q missing %s", got, want)
		}
	}
	if !sigOK {
		t.Error("webhook signature or custom header wrong")
	}
}

func TestMapShapes(t *testing.T) {
	site, api := testSite(t), jobServer(t, "")
	v2 := call(t, "POST", api.URL+"/v2/map", fmt.Sprintf(`{"url":%q,"limit":5}`, site.URL+"/docs"))
	links := v2["links"].([]any)
	if v2["success"] != true || len(links) != 5 {
		t.Fatalf("v2 map: %v", v2)
	}
	if _, ok := links[0].(map[string]any)["url"]; !ok {
		t.Fatalf("v2 links must be objects: %v", links[0])
	}
	v1 := call(t, "POST", api.URL+"/v1/map", fmt.Sprintf(`{"url":%q,"limit":3}`, site.URL+"/docs"))
	if _, ok := v1["links"].([]any)[0].(string); !ok || len(v1["links"].([]any)) != 3 {
		t.Fatalf("v1 links must be strings: %v", v1)
	}
}
