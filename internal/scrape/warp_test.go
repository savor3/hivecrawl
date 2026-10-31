package scrape

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fakeWarp answers every request with a normal page, as WARP would when the
// site only blocked the node's own IP.
type fakeWarp struct{ calls int }

func (f *fakeWarp) RoundTrip(r *http.Request) (*http.Response, error) {
	f.calls++
	return &http.Response{
		StatusCode: 200, Header: http.Header{"Content-Type": {"text/html"}}, Request: r,
		Body: io.NopCloser(strings.NewReader("<html><head><title>Fares</title></head><body><main><p>Fare is 20 rupees</p></main></body></html>")),
	}, nil
}

func TestBlockedPageRetriedThroughWarp(t *testing.T) {
	for name, handler := range map[string]http.HandlerFunc{
		"403": func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(403) },
		"cloudflare challenge": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			w.Write([]byte("<html><head><title>Just a moment...</title></head><body>challenge-platform</body></html>"))
		},
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(handler)
			defer srv.Close()
			warp := &fakeWarp{}
			s := &Scraper{Direct: StdClient(&http.Client{}), Warp: StdClient(&http.Client{Transport: warp})}
			doc, err := s.Scrape(context.Background(), srv.URL+"/fares", Options{OnlyMainContent: true})
			if err != nil {
				t.Fatal(err)
			}
			if warp.calls != 1 || doc.Metadata.ProxyUsed != "stealth" || !strings.Contains(doc.Markdown, "Fare is 20 rupees") {
				t.Fatalf("warp calls %d, proxyUsed %q, markdown %q", warp.calls, doc.Metadata.ProxyUsed, doc.Markdown)
			}
		})
	}
}

func TestNormalPageDoesNotUseWarp(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte("<html><body><p>hello</p></body></html>"))
	}))
	defer srv.Close()
	warp := &fakeWarp{}
	doc, err := (&Scraper{Direct: StdClient(&http.Client{}), Warp: StdClient(&http.Client{Transport: warp})}).Scrape(context.Background(), srv.URL, Options{})
	if err != nil || warp.calls != 0 || doc.Metadata.ProxyUsed != "basic" {
		t.Fatalf("err %v, warp calls %d, proxy %q", err, warp.calls, doc.Metadata.ProxyUsed)
	}
}

func TestInternalAddressNeverRetriedThroughWarp(t *testing.T) {
	warp := &fakeWarp{}
	s := &Scraper{Direct: StdClient(Client(nilDialer())), Warp: StdClient(&http.Client{Transport: warp})}
	_, err := s.Scrape(context.Background(), "http://127.0.0.1:9/", Options{})
	var se *Error
	if !asErr(err, &se) || se.Code != "SCRAPE_ADDRESS_NOT_ALLOWED" || warp.calls != 0 {
		t.Fatalf("err %v, warp calls %d", err, warp.calls)
	}
}

func nilDialer() *net.Dialer { return &net.Dialer{} }

func TestBrowserFeaturesNeedABrowser(t *testing.T) {
	s := &Scraper{Direct: StdClient(&http.Client{})}
	for _, o := range []Options{{Formats: []string{"screenshot"}}, {WaitFor: time.Second}, {Mobile: true}} {
		_, err := s.Scrape(context.Background(), "https://example.com", o)
		var se *Error
		if !asErr(err, &se) || se.Code != "SCRAPE_BROWSER_UNAVAILABLE" {
			t.Fatalf("%+v: want SCRAPE_BROWSER_UNAVAILABLE, got %v", o, err)
		}
	}
}

func TestBlockedEverywhereFallsBackToPeers(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(403) }))
	defer srv.Close()
	asked := ""
	s := &Scraper{Direct: StdClient(&http.Client{}), Peers: func(ctx context.Context, u string) (*Raw, string, error) {
		asked = u
		return &Raw{Status: 200, URL: u, ContentType: "text/html", Body: []byte("<html><body><main><p>via a peer</p></main></body></html>")}, "home", nil
	}}
	doc, err := s.Scrape(context.Background(), srv.URL+"/x", Options{OnlyMainContent: true})
	if err != nil {
		t.Fatal(err)
	}
	if asked != srv.URL+"/x" || !strings.Contains(doc.Markdown, "via a peer") || doc.Metadata.ProxyUsed != "stealth" {
		t.Fatalf("asked=%q doc=%+v", asked, doc.Metadata)
	}
}

func TestPeersNotUsedForNormalOrInternalPages(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("<html><body>ok</body></html>")) }))
	defer srv.Close()
	calls := 0
	peers := func(ctx context.Context, u string) (*Raw, string, error) { calls++; return nil, "", nil }
	if _, err := (&Scraper{Direct: StdClient(&http.Client{}), Peers: peers}).Scrape(context.Background(), srv.URL, Options{}); err != nil {
		t.Fatal(err)
	}
	(&Scraper{Direct: StdClient(Client(nilDialer())), Peers: peers}).Scrape(context.Background(), "http://127.0.0.1:9/", Options{})
	if calls != 0 {
		t.Fatalf("peers asked %d times", calls)
	}
}

// dnsFail fails every request with a DNS error, as a resolver would.
type dnsFail struct{ err *net.DNSError }

func (d dnsFail) RoundTrip(r *http.Request) (*http.Response, error) { return nil, d.err }

// A DNS timeout says nothing about the site, so the other routes are tried;
// only a real "no such domain" answer ends the scrape at once.
func TestOnlyMissingDomainsSkipTheOtherRoutes(t *testing.T) {
	for name, c := range map[string]struct {
		err      *net.DNSError
		wantWarp int
	}{
		"timeout":   {&net.DNSError{Err: "i/o timeout", Name: "example.com", IsTimeout: true}, 1},
		"servfail":  {&net.DNSError{Err: "server misbehaving", Name: "example.com", IsTemporary: true}, 1},
		"no domain": {&net.DNSError{Err: "no such host", Name: "example.com", IsNotFound: true}, 0},
	} {
		t.Run(name, func(t *testing.T) {
			warp := &fakeWarp{}
			s := &Scraper{Direct: StdClient(&http.Client{Transport: dnsFail{c.err}}), Warp: StdClient(&http.Client{Transport: warp})}
			_, err := s.Scrape(context.Background(), "https://example.com/fares", Options{OnlyMainContent: true})
			if warp.calls != c.wantWarp {
				t.Fatalf("warp calls %d, want %d (err %v)", warp.calls, c.wantWarp, err)
			}
			var se *Error
			if c.wantWarp == 0 && (!errors.As(err, &se) || se.Code != "SCRAPE_DNS_RESOLUTION_ERROR") {
				t.Fatalf("missing domain: got %v", err)
			}
			if c.wantWarp == 1 && err != nil {
				t.Fatalf("site reachable through WARP, got %v", err)
			}
		})
	}
}
