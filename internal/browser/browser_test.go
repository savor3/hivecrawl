package browser

import (
	"context"
	"encoding/base64"
	"errors"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

// Integration tests against a real headless Chrome:
//
//	docker run -d --rm -p 127.0.0.1:19222:9222 chromedp/headless-shell:155.0.8059.40
//	HC_TEST_BROWSER=127.0.0.1:19222 go test ./internal/browser/
func testBrowser(t *testing.T) *Browser {
	addr := os.Getenv("HC_TEST_BROWSER")
	if addr == "" {
		t.Skip("set HC_TEST_BROWSER=host:port of a headless Chrome")
	}
	return New(addr, 2, nil)
}

func dataPage(html string) string { return "data:text/html," + url.PathEscape(html) }

func TestRendersJavaScript(t *testing.T) {
	b := testBrowser(t)
	page := dataPage(`<html><body><div id="app"></div><script>
		setTimeout(() => { document.getElementById('app').innerHTML = '<h1>Rendered by JS</h1><button id="b" onclick="this.textContent=\'clicked\'">go</button><input id="q">'; }, 200);
	</script></body></html>`)
	res, err := b.Render(context.Background(), page, Options{
		WaitFor: 500 * time.Millisecond,
		Actions: []Action{
			{Type: "wait", Selector: "#b"},
			{Type: "click", Selector: "#b"},
			{Type: "click", Selector: "#q"},
			{Type: "write", Text: "kochi"},
			{Type: "executeJavascript", Script: "document.getElementById('q').value + '/' + document.getElementById('b').textContent"},
			{Type: "screenshot"},
			{Type: "scrape"},
			{Type: "pdf", Format: "A4"},
		},
		Screenshot: &Screenshot{FullPage: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.HTML, "Rendered by JS") {
		t.Fatalf("JS content missing: %s", res.HTML[:min(300, len(res.HTML))])
	}
	a := res.Actions
	if len(a.JavascriptReturns) != 1 || a.JavascriptReturns[0].Value != "kochi/clicked" || a.JavascriptReturns[0].Type != "string" {
		t.Fatalf("javascriptReturns = %+v", a.JavascriptReturns)
	}
	if len(a.Screenshots) != 1 || len(a.Scrapes) != 1 || len(a.PDFs) != 1 {
		t.Fatalf("actions result = %d screenshots, %d scrapes, %d pdfs", len(a.Screenshots), len(a.Scrapes), len(a.PDFs))
	}
	png, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(res.Screenshot, "data:image/png;base64,"))
	if err != nil || !strings.HasPrefix(string(png), "\x89PNG") {
		t.Fatalf("screenshot is not a PNG data URL: %.60s", res.Screenshot)
	}
	pdf, _ := base64.StdEncoding.DecodeString(strings.TrimPrefix(a.PDFs[0], "data:application/pdf;base64,"))
	if !strings.HasPrefix(string(pdf), "%PDF") {
		t.Fatal("pdf action did not return a PDF")
	}
}

func TestBlocksInternalAddresses(t *testing.T) {
	b := testBrowser(t)
	if _, err := b.Render(context.Background(), "http://169.254.169.254/latest/meta-data/", Options{}); !errors.Is(err, ErrAddressNotAllowed) {
		t.Fatalf("page URL on an internal address: want ErrAddressNotAllowed, got %v", err)
	}
	// A public page (here: data:) that loads internal sub-resources.
	page := dataPage(`<html><body><img src="http://127.0.0.1:9222/json/version"><img src="http://10.0.0.1/x.png">
		<script>fetch('http://192.168.1.1/admin').catch(() => {});</script></body></html>`)
	res, err := b.Render(context.Background(), page, Options{WaitFor: 500 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(res.Blocked, " ")
	for _, want := range []string{"127.0.0.1:9222", "10.0.0.1", "192.168.1.1"} {
		if !strings.Contains(joined, want) {
			t.Errorf("request to %s was not blocked (blocked: %v)", want, res.Blocked)
		}
	}
}

func TestMobileEmulation(t *testing.T) {
	b := testBrowser(t)
	res, err := b.Render(context.Background(), dataPage(`<html><head><meta name="viewport" content="width=device-width"></head><body><script>document.body.textContent = innerWidth + ' ' + navigator.userAgent;</script></body></html>`), Options{Mobile: true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.HTML, "412 ") || !strings.Contains(res.HTML, "Android") {
		t.Fatalf("mobile emulation not applied: %s", res.HTML)
	}
}

func TestValidate(t *testing.T) {
	bad := [][]Action{
		{{Type: "wait"}},
		{{Type: "wait", Milliseconds: 10, Selector: "#a"}},
		{{Type: "click"}},
		{{Type: "teleport"}},
		{{Type: "wait", Milliseconds: 61000}},
	}
	for i, a := range bad {
		if err := Validate(a, 0); err == nil {
			t.Errorf("case %d accepted: %+v", i, a)
		}
	}
	if err := Validate([]Action{{Type: "wait", Milliseconds: 500}, {Type: "click", Selector: "a"}, {Type: "screenshot"}}, time.Second); err != nil {
		t.Fatal(err)
	}
}
