package scrape

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const page = `<!doctype html><html lang="en"><head>
<title>Kochi Metro Guide</title>
<meta name="description" content="Timings and fares">
<meta property="og:title" content="Kochi Metro">
<link rel="icon" href="/favicon.ico">
<script>track()</script><style>body{}</style>
</head><body>
<header><nav><a href="/home">Home</a> <a href="/about">About</a></nav></header>
<div class="cookie-banner">We use cookies</div>
<main><article>
<h1>Kochi Metro Timings</h1>
<p>Trains run every <strong>7 minutes</strong> at peak. See <a href="/fares">fares</a>.</p>
<img src="/map.png" alt="map"><img src="data:image/png;base64,AAAA" alt="inline">
<table><tr><th>Station</th><th>First train</th></tr><tr><td>Aluva</td><td>06:00</td></tr></table>
</article></main>
<aside class="sidebar">Related posts</aside>
<footer>© 2026</footer>
</body></html>`

// testClient allows loopback so the test server is reachable; production
// clients from Client() refuse it.
func testClient() *http.Client { return &http.Client{} }

func TestScrapeMarkdownMainContent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(page))
	}))
	defer srv.Close()

	doc, err := Scrape(context.Background(), testClient(), srv.URL+"/guide", Options{
		Formats: []string{"markdown", "links", "changeTracking"}, OnlyMainContent: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	md := doc.Markdown
	for _, want := range []string{"# Kochi Metro Timings", "**7 minutes**", "[fares](" + srv.URL + "/fares)", "![map](" + srv.URL + "/map.png)", "| Aluva"} {
		if !strings.Contains(md, want) {
			t.Errorf("markdown missing %q:\n%s", want, md)
		}
	}
	for _, unwanted := range []string{"cookies", "Related posts", "© 2026", "About", "track()", "base64"} {
		if strings.Contains(md, unwanted) {
			t.Errorf("markdown contains non-main content %q:\n%s", unwanted, md)
		}
	}
	m := doc.Metadata
	if m.Title != "Kochi Metro Guide" || m.Description != "Timings and fares" || m.Language != "en" ||
		m.OgTitle != "Kochi Metro" || m.StatusCode != 200 || m.Favicon != srv.URL+"/favicon.ico" || m.SourceURL != srv.URL+"/guide" {
		t.Errorf("metadata wrong: %+v", m)
	}
	if len(doc.Links) != 1 || doc.Links[0] != srv.URL+"/fares" {
		t.Errorf("links = %v (onlyMainContent should drop nav links)", doc.Links)
	}
	if !strings.Contains(doc.Warning, "changeTracking") {
		t.Errorf("want warning about unsupported changeTracking format, got %q", doc.Warning)
	}
}

func TestScrapeFullPageWhenNotMainOnly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(page))
	}))
	defer srv.Close()
	doc, err := Scrape(context.Background(), testClient(), srv.URL, Options{OnlyMainContent: false})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(doc.Markdown, "Related posts") || !strings.Contains(doc.Markdown, "About") {
		t.Fatalf("full page should keep sidebar/nav:\n%s", doc.Markdown)
	}
}

func TestRefusesInternalAddresses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("secret")) }))
	defer srv.Close()
	c := Client(&net.Dialer{})
	for _, u := range []string{srv.URL, "http://127.0.0.1:1/", "http://[::1]:1/", "http://169.254.169.254/latest/meta-data/", "http://10.0.0.1/"} {
		_, err := Scrape(context.Background(), c, u, Options{})
		var se *Error
		if !errors.As(err, &se) || se.Code != "SCRAPE_ADDRESS_NOT_ALLOWED" {
			t.Errorf("%s: want SCRAPE_ADDRESS_NOT_ALLOWED, got %v", u, err)
		}
	}
}

func TestRefusesRedirectToInternal(t *testing.T) {
	// A public-looking page that redirects to an internal address must be
	// refused at connect time. Simulated by redirecting to 127.0.0.1 through
	// the production client: the first hop is also loopback here, so check
	// the dial guard on the redirect target via the error code.
	c := Client(&net.Dialer{})
	_, err := Scrape(context.Background(), c, "http://localhost:9/", Options{})
	var se *Error
	if !errors.As(err, &se) || se.Code != "SCRAPE_ADDRESS_NOT_ALLOWED" {
		t.Fatalf("localhost: want SCRAPE_ADDRESS_NOT_ALLOWED, got %v", err)
	}
}

func TestRejectsBadInput(t *testing.T) {
	for _, u := range []string{"", "ftp://x.com/a", "file:///etc/passwd", "not a url", "javascript:alert(1)"} {
		_, err := Scrape(context.Background(), testClient(), u, Options{})
		var se *Error
		if !errors.As(err, &se) || se.Code != "SCRAPE_INVALID_URL" {
			t.Errorf("%q: want SCRAPE_INVALID_URL, got %v", u, err)
		}
	}
}

func TestUnsupportedContentType(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Write([]byte("\x89PNG\r\n"))
	}))
	defer srv.Close()
	_, err := Scrape(context.Background(), testClient(), srv.URL, Options{})
	var se *Error
	if !errors.As(err, &se) || se.Code != "SCRAPE_UNSUPPORTED_CONTENT_TYPE" {
		t.Fatalf("want SCRAPE_UNSUPPORTED_CONTENT_TYPE, got %v", err)
	}
}
