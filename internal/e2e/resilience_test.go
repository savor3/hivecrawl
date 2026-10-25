package e2e

// Regression tests for a burst of ~100 news searches from a home node
// whose engines all got CAPTCHA'd / rate limited, with WARP timing out:
// two thirds came back empty because (1) the home node could no longer
// answer verification probes, so peers held it to the 3/min trial rate and
// answered 429, (2) news searches had no WARP retry, and (3) nothing
// served an older answer when every route failed.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"hivecrawl/internal/mesh"
	"hivecrawl/internal/types"
)

// anyResult is a v2 search response with web or news results.
type anyResult struct {
	Success bool   `json:"success"`
	Error   string `json:"error"`
	Data    struct {
		Web  []struct{ URL string } `json:"web"`
		News []struct{ URL string } `json:"news"`
	} `json:"data"`
	Source string
}

func (r anyResult) n() int { return len(r.Data.Web) + len(r.Data.News) }

func searchJSON(t *testing.T, base, body string) anyResult {
	t.Helper()
	resp, err := http.Post(base+"/v2/search", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var sr anyResult
	if err := json.NewDecoder(resp.Body).Decode(&sr); err != nil {
		t.Fatal(err)
	}
	sr.Source = resp.Header.Get("X-HC-Source")
	return sr
}

// everyEngine is every direct engine and WARP copy a node may ask for.
func everyEngine() []string {
	var all []string
	for _, e := range append(append([]string{}, allEngines...),
		"google news", "brave.news", "duckduckgo news", "yahoo news",
		"google images", "brave.images", "duckduckgo images") {
		all = append(all, e, e+" warp")
	}
	return all
}

// probeQueries mirrors mesh.probeQueries.
var probeQueries = []string{
	"weather forecast", "world news", "wikipedia", "python tutorial", "football scores",
	"currency exchange rates", "easy recipes", "linux commands", "time zones", "public holidays",
}

// A home node that worked yesterday and restarts today with every route
// blocked (direct and WARP) must still be served by peers for a whole
// burst, not cut to the trial rate after 3 searches. Its only proof is the
// probe answers it saved while its engines worked.
func TestBlockedHomeNodeKeepsMeshAccess(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// What fakeSearx returned for each probe query while home worked.
	dir := t.TempDir()
	saved := map[string]any{}
	for _, q := range probeQueries {
		u := fmt.Sprintf("https://google-cse.example/%s/0", strings.ReplaceAll(q, " ", "-"))
		saved[q] = map[string]any{"urls": []string{u}, "at": time.Now().Add(-20 * time.Hour)}
	}
	b, _ := json.Marshal(saved)
	if err := os.WriteFile(filepath.Join(dir, "probe-answers.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	blocked := map[string]bool{}
	for _, e := range everyEngine() {
		blocked[e] = true
	}

	a := start(t, ctx, "public-a", true, nil, netKey, engineSpec{per: 10})
	home := start(t, ctx, "home", false, []string{a.url}, netKey, engineSpec{per: 10, blocked: blocked, dataDir: dir})
	waitFor(t, "home tunnel on A", func() bool { return hasPeer(a, "home", func(p mesh.Peer) bool { return p.Tunneled }) })

	// The home node has no listener (NAT), so search it in-process: web
	// and news, the two paths the burst used. 20 searches at once is far
	// past the 3/min trial allowance.
	for i := range 20 {
		req := types.SearchRequest{Query: fmt.Sprintf("burst query %d", i), Limit: 5}
		if i%2 == 1 {
			req.Category, req.TimeRange = "news", "day"
		}
		r := home.node.Search(ctx, req)
		if !r.Success || len(r.Data.Web) == 0 {
			t.Fatalf("search %d (%+v) came back empty: source=%q err=%q", i, req, r.Meta.Source, r.Error)
		}
		if !strings.Contains(r.Meta.Source, "peer:public-a") {
			t.Fatalf("search %d: want an answer from the peer, got source %q", i, r.Meta.Source)
		}
		if i == 2 {
			time.Sleep(300 * time.Millisecond) // let A's background probe of home finish
		}
	}
}

// News searches whose engines are blocked retry the WARP copies, like web
// searches do.
func TestNewsSearchRetriesWarp(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a := start(t, ctx, "public-a", true, nil, netKey, engineSpec{per: 3, blocked: map[string]bool{
		"google cse": true, "brave": true, "duckduckgo": true,
	}})
	r := searchJSON(t, a.url, `{"query":"kerala rain","limit":5,"sources":[{"type":"news"}]}`)
	if !r.Success || r.n() == 0 {
		t.Fatalf("news search empty: source=%q err=%q", r.Source, r.Error)
	}
	if r.Source != "warp" {
		t.Fatalf("source = %q, want warp", r.Source)
	}
	var asked []string
	for _, e := range a.sx.askedEngines("kerala rain") {
		asked = append(asked, strings.Join(e, ","))
	}
	if !strings.Contains(strings.Join(asked, " | "), "brave warp") {
		t.Fatalf("WARP copies never asked: %v", asked)
	}
}

// When no route can answer, an older cached answer is served instead of
// an empty result.
func TestStaleCacheWhenEveryRouteFails(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a := start(t, ctx, "public-a", true, nil, netKey, engineSpec{per: 10, ttl: 50 * time.Millisecond})
	for _, body := range []string{
		`{"query":"sensex today","limit":5}`,
		`{"query":"sensex today","limit":5,"sources":[{"type":"news"}],"tbs":"qdr:d"}`,
	} {
		if r := searchJSON(t, a.url, body); r.n() == 0 {
			t.Fatalf("warm-up %s: empty", body)
		}
	}
	time.Sleep(80 * time.Millisecond) // past the TTL
	a.sx.block(everyEngine()...)
	for _, body := range []string{
		`{"query":"sensex today","limit":5}`,
		`{"query":"sensex today","limit":5,"sources":[{"type":"news"}],"tbs":"qdr:d"}`,
	} {
		r := searchJSON(t, a.url, body)
		if r.n() == 0 {
			t.Fatalf("%s: empty, want the stale cached answer (source=%q err=%q)", body, r.Source, r.Error)
		}
		if !strings.HasPrefix(r.Source, "stale:") {
			t.Fatalf("%s: source = %q, want stale:*", body, r.Source)
		}
	}
}

// With nothing cached and every route blocked, the API says "retry later"
// (HTTP 429 + Retry-After) instead of a success that looks like "no
// results", so clients know the query was not answered.
func TestUnanswerableSearchSaysRetry(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a := start(t, ctx, "public-a", true, nil, netKey, engineSpec{per: 10})
	a.sx.block(everyEngine()...)
	for _, body := range []string{
		`{"query":"never asked before","limit":5}`,
		`{"query":"never asked before","limit":5,"sources":[{"type":"news"}],"tbs":"qdr:d"}`,
	} {
		resp, err := http.Post(a.url+"/v2/search", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusTooManyRequests || resp.Header.Get("Retry-After") == "" {
			t.Fatalf("%s: status %d Retry-After %q, want 429 with Retry-After", body, resp.StatusCode, resp.Header.Get("Retry-After"))
		}
	}
	// A query that genuinely has no results (engines fine) stays a 200.
	a.sx.block()
	a.sx.mu.Lock()
	a.sx.spec.per = 0
	a.sx.mu.Unlock()
	resp, err := http.Post(a.url+"/v2/search", "application/json", strings.NewReader(`{"query":"zxqv nothing","limit":5}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("genuinely empty query: status %d, want 200", resp.StatusCode)
	}
}
