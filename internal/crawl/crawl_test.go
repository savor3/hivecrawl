package crawl

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"

	"hivecrawl/internal/scrape"
)

// site serves a small website:
//
//	/docs        -> links to /docs/a, /docs/b, /docs/private, /blog/x, https://external.example/
//	/docs/a      -> links to /docs/a/deep, /docs/a/ (duplicate of /docs/a)
//	/docs/c      -> only in the sitemap
//	robots.txt disallows /docs/private
func site(t *testing.T) *httptest.Server {
	var srv *httptest.Server
	page := func(title string, links ...string) string {
		var b strings.Builder
		fmt.Fprintf(&b, "<html><head><title>%s</title></head><body><main><h1>%s</h1>", title, title)
		for _, l := range links {
			fmt.Fprintf(&b, `<a href="%s">%s</a> `, l, l)
		}
		b.WriteString("</main></body></html>")
		return b.String()
	}
	pages := map[string]string{
		"/docs":         page("Docs", "/docs/a", "/docs/b", "/docs/private", "/blog/x", "https://external.example/"),
		"/docs/a":       page("A", "/docs/a/deep", "/docs/a/"),
		"/docs/a/deep":  page("Deep"),
		"/docs/b":       page("B"),
		"/docs/c":       page("C"),
		"/docs/private": page("Private"),
		"/blog/x":       page("Blog X", "/docs"),
	}
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/robots.txt":
			fmt.Fprintf(w, "User-agent: *\nDisallow: /docs/private\nSitemap: %s/sitemap.xml\n", srv.URL)
			return
		case "/sitemap.xml":
			w.Header().Set("Content-Type", "application/xml")
			fmt.Fprintf(w, `<?xml version="1.0"?><urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9"><url><loc>%s/docs/c</loc></url><url><loc>%s/blog/x</loc></url></urlset>`, srv.URL, srv.URL)
			return
		}
		p := strings.TrimSuffix(r.URL.Path, "/")
		if body, ok := pages[p]; ok {
			w.Header().Set("Content-Type", "text/html")
			w.Write([]byte(body))
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func scraper() *scrape.Scraper { return &scrape.Scraper{Direct: scrape.StdClient(&http.Client{})} }

func crawl(t *testing.T, start string, o Options) (pages []string, robotsBlocked []string) {
	var mu sync.Mutex
	err := Run(context.Background(), scraper(), start, o, Callbacks{
		Page: func(d *scrape.Document) {
			mu.Lock()
			pages = append(pages, strings.TrimPrefix(d.Metadata.URL, start[:strings.Index(start[8:], "/")+8]))
			mu.Unlock()
		},
		RobotsBlocked: func(u string) { mu.Lock(); robotsBlocked = append(robotsBlocked, u); mu.Unlock() },
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(pages)
	return pages, robotsBlocked
}

func TestCrawlChildrenRobotsAndSitemap(t *testing.T) {
	srv := site(t)
	pages, blocked := crawl(t, srv.URL+"/docs", Options{MaxDiscoveryDepth: -1, Limit: 100, DeduplicateSimilarURLs: true})
	want := []string{"/docs", "/docs/a", "/docs/a/deep", "/docs/b", "/docs/c"}
	if !slices.Equal(pages, want) {
		t.Fatalf("pages = %v, want %v (children only, sitemap page included, blog excluded)", pages, want)
	}
	if len(blocked) != 1 || !strings.HasSuffix(blocked[0], "/docs/private") {
		t.Fatalf("robots-blocked = %v", blocked)
	}
}

func TestCrawlOptions(t *testing.T) {
	srv := site(t)
	cases := []struct {
		name string
		o    Options
		want []string
	}{
		{"depth 1", Options{MaxDiscoveryDepth: 1, Sitemap: "skip"}, []string{"/docs", "/docs/a", "/docs/b"}},
		{"limit 2", Options{MaxDiscoveryDepth: -1, Limit: 2, Sitemap: "skip"}, nil}, // checked by count
		{"exclude /docs/a", Options{MaxDiscoveryDepth: -1, Sitemap: "skip", ExcludePaths: []string{"^/docs/a"}}, []string{"/docs", "/docs/b"}},
		{"include b", Options{MaxDiscoveryDepth: -1, Sitemap: "skip", IncludePaths: []string{"/b$"}}, []string{"/docs", "/docs/b"}},
		{"entire domain", Options{MaxDiscoveryDepth: -1, Sitemap: "skip", CrawlEntireDomain: true}, []string{"/blog/x", "/docs", "/docs/a", "/docs/a/deep", "/docs/b"}},
		{"ignore robots", Options{MaxDiscoveryDepth: 1, Sitemap: "skip", IgnoreRobotsTxt: true}, []string{"/docs", "/docs/a", "/docs/b", "/docs/private"}},
		{"sitemap only", Options{MaxDiscoveryDepth: -1, Sitemap: "only", CrawlEntireDomain: true}, []string{"/blog/x", "/docs/c"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.o.Limit == 0 {
				c.o.Limit = 100
			}
			c.o.DeduplicateSimilarURLs = true
			pages, _ := crawl(t, srv.URL+"/docs", c.o)
			if c.want == nil {
				if len(pages) != c.o.Limit {
					t.Fatalf("got %d pages, want limit %d: %v", len(pages), c.o.Limit, pages)
				}
				return
			}
			if !slices.Equal(pages, c.want) {
				t.Fatalf("pages = %v, want %v", pages, c.want)
			}
		})
	}
}

func TestCrawlBadPattern(t *testing.T) {
	srv := site(t)
	err := Run(context.Background(), scraper(), srv.URL, Options{IncludePaths: []string{"("}}, Callbacks{})
	if err == nil || !strings.Contains(err.Error(), "invalid path pattern") {
		t.Fatalf("want invalid pattern error, got %v", err)
	}
}

func TestCrawlCancel(t *testing.T) {
	srv := site(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := Run(ctx, scraper(), srv.URL+"/docs", Options{MaxDiscoveryDepth: -1}, Callbacks{}); err == nil {
		t.Fatal("cancelled crawl returned no error")
	}
}

func TestMap(t *testing.T) {
	srv := site(t)
	search := func(ctx context.Context, q string, limit int) ([]Link, error) {
		if !strings.Contains(q, "site:127.0.0.1") {
			t.Errorf("search query %q lacks site:", q)
		}
		return []Link{{URL: srv.URL + "/docs/b", Title: "B from search", Description: "found by search"}}, nil
	}
	links, err := Map(context.Background(), scraper(), search, srv.URL+"/docs", MapOptions{IncludeSubdomains: true, IgnoreQueryParameters: true})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Link{}
	for _, l := range links {
		got[strings.TrimPrefix(l.URL, srv.URL)] = l
	}
	for _, want := range []string{"/docs", "/docs/a", "/docs/b", "/docs/c", "/blog/x"} {
		if _, ok := got[want]; !ok {
			t.Errorf("map missing %s (got %v)", want, links)
		}
	}
	if _, ok := got["https://external.example/"]; ok {
		t.Error("map included an external link")
	}
	if got["/docs"].Title != "Docs" || got["/docs/b"].Title != "B from search" {
		t.Errorf("titles: docs=%q b=%q", got["/docs"].Title, got["/docs/b"].Title)
	}
	ranked, _ := Map(context.Background(), scraper(), search, srv.URL+"/docs", MapOptions{Search: "blog", Sitemap: "include", Limit: 2})
	if len(ranked) != 2 || !strings.Contains(ranked[0].URL, "/blog/") {
		t.Fatalf("search ranking/limit: %v", ranked)
	}
}
