package mesh

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"hivecrawl/internal/netx"
	"hivecrawl/internal/types"
)

func scrapeNode(t *testing.T, serve bool, seeds ...string) (*Mesh, string) {
	var m *Mesh
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mux := http.NewServeMux()
		m.Register(mux)
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	d := netx.PublicDialer(nil)
	m = New(Options{Key: testKey, PeerRate: 100, PublicURL: srv.URL, Peers: seeds, Dialer: d, Client: netx.Client(d, 5*time.Second),
		Local: results, NoIPLimits: true, ServeScrapes: serve,
		FetchRaw: func(ctx context.Context, u string) (*types.RawPage, error) {
			return &types.RawPage{Status: 200, URL: u, ContentType: "text/html", Body: []byte("<html><body>fetched by peer</body></html>")}, nil
		}})
	return m, srv.URL
}

func TestFetchViaPeerThatOptedIn(t *testing.T) {
	_, helperURL := scrapeNode(t, true)
	asker, _ := scrapeNode(t, false, helperURL)
	asker.gossipOnce(context.Background())
	page, name, err := asker.FetchViaPeers(context.Background(), "https://example.com/blocked")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(page.Body), "fetched by peer") || page.URL != "https://example.com/blocked" || name == "" {
		t.Fatalf("page=%+v name=%q", page, name)
	}
}

func TestNoFetchFromPeerThatDidNotOptIn(t *testing.T) {
	_, plainURL := scrapeNode(t, false)
	asker, _ := scrapeNode(t, false, plainURL)
	asker.gossipOnce(context.Background())
	if _, _, err := asker.FetchViaPeers(context.Background(), "https://example.com/"); err == nil {
		t.Fatal("fetched through a peer that does not offer it")
	}
	resp, err := http.Post(plainURL+"/mesh/v1/scrape", "application/json", strings.NewReader(`{"url":"https://example.com"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("non-serving node exposes /mesh/v1/scrape: %d", resp.StatusCode)
	}
}
