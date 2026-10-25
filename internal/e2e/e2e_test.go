// Package e2e runs several real nodes in-process: public nodes with some
// engines blocked, and a home node behind "NAT" (it has no listener at all
// and only dials out). It checks the full flow: local first, then blocked
// engines scattered in parallel to WARP and to peers (via tunnel and relay),
// then merged.
package e2e

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"hivecrawl/internal/api"
	"hivecrawl/internal/cache"
	"hivecrawl/internal/mesh"
	"hivecrawl/internal/netx"
	"hivecrawl/internal/node"
	"hivecrawl/internal/searx"
)

const netKey = "e2e-network-key-0123456789"

var allEngines = []string{"google cse", "brave", "duckduckgo"}

// engineSpec describes one fake node's SearXNG.
type engineSpec struct {
	down    bool            // SearXNG itself returns 500
	per     int             // results per working engine
	blocked map[string]bool // engines (incl. "<x> warp") that report CAPTCHA
	ttl     time.Duration   // result cache TTL (default an hour)
	dataDir string          // mesh state dir (default a fresh temp dir)
}

// fakeSearx emulates SearXNG: without engines= it runs allEngines; with
// engines= only those. Each working engine contributes spec.per results.
type fakeSearx struct {
	spec  engineSpec
	calls atomic.Int32
	mu    sync.Mutex
	asked [][]string // engines= of each call (nil = all)
	query []string   // q of each call
}

// block changes which engines report CAPTCHA, mid-test.
func (f *fakeSearx) block(engines ...string) {
	m := map[string]bool{}
	for _, e := range engines {
		m[e] = true
	}
	f.mu.Lock()
	f.spec.blocked = m
	f.mu.Unlock()
}

func (f *fakeSearx) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.calls.Add(1)
	f.mu.Lock()
	spec := f.spec
	f.mu.Unlock()
	if spec.down {
		http.Error(w, "down", http.StatusInternalServerError)
		return
	}
	var engines []string
	if e := r.URL.Query().Get("engines"); e != "" {
		engines = strings.Split(e, ",")
	}
	f.mu.Lock()
	f.asked = append(f.asked, engines)
	f.query = append(f.query, r.URL.Query().Get("q"))
	f.mu.Unlock()
	if engines == nil {
		engines = allEngines
	}
	q := strings.ReplaceAll(r.URL.Query().Get("q"), " ", "-")
	var res []map[string]any
	var unresp [][]string
	for _, e := range engines {
		if spec.blocked[e] {
			unresp = append(unresp, []string{e, "CAPTCHA"})
			continue
		}
		for i := range spec.per {
			res = append(res, map[string]any{
				"url":   fmt.Sprintf("https://%s.example/%s/%d", strings.ReplaceAll(e, " ", "-"), q, i),
				"title": e + " result", "content": "about " + q, "engines": []string{e},
			})
		}
	}
	json.NewEncoder(w).Encode(map[string]any{"results": res, "unresponsive_engines": unresp})
}

// askedEngines lists engines= for calls with query q ("" = all calls;
// probes from the proof-of-participation check also reach SearXNG).
func (f *fakeSearx) askedEngines(q ...string) [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out [][]string
	for i, e := range f.asked {
		if len(q) == 0 || f.query[i] == q[0] {
			out = append(out, e)
		}
	}
	return out
}

type testNode struct {
	node *node.Node
	sx   *fakeSearx
	url  string // empty for the home node
	// dataDir holds the mesh's saved state (peers, probe answers).
	dataDir string
}

func start(t *testing.T, ctx context.Context, name string, public bool, peers []string, key string, spec engineSpec) *testNode {
	tn := &testNode{sx: &fakeSearx{spec: spec}, dataDir: cmp.Or(spec.dataDir, t.TempDir())}
	// Runs after the servers below close (cleanups run in reverse), before
	// the data dir is removed: background saves must be done by then.
	t.Cleanup(func() { tn.node.Mesh.Wait() })
	sxSrv := httptest.NewServer(tn.sx)
	t.Cleanup(sxSrv.Close)
	n := node.New(name, cache.New(cmp.Or(spec.ttl, time.Hour), 100), searx.New(sxSrv.URL, http.DefaultClient), 8)
	n.Engines, n.Warp = allEngines, true
	tn.node = n

	var handler http.Handler
	if public {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { handler.ServeHTTP(w, r) }))
		t.Cleanup(srv.Close)
		tn.url = srv.URL
	}
	d := netx.PublicDialer(nil)
	n.Mesh = mesh.New(mesh.Options{
		Name: name, PublicURL: tn.url, GossipEvery: 200 * time.Millisecond, Peers: peers, Key: key, PeerRate: 100,
		Dialer: d, Client: netx.Client(d, 5*time.Second), Local: n.Local, ProbeLocal: n.ProbeAnswer, Blocked: n.BlockedEngines,
		DataDir: tn.dataDir,
	})
	mux := http.NewServeMux()
	(&api.Server{Node: n, PublicURL: tn.url, Started: time.Now()}).Register(mux)
	n.Mesh.Register(mux)
	handler = mux
	n.Mesh.Start(ctx)
	return tn
}

// result is a Firecrawl v2 response plus HiveCrawl's X-HC-Source header.
type result struct {
	Success bool   `json:"success"`
	Error   string `json:"error"`
	Data    struct {
		Web []struct {
			URL      string `json:"url"`
			Position int    `json:"position"`
		} `json:"web"`
	} `json:"data"`
	Source string
}

func (r result) urls() string {
	var b strings.Builder
	for _, w := range r.Data.Web {
		b.WriteString(w.URL + " ")
	}
	return b.String()
}

func search(t *testing.T, base, q string) result {
	t.Helper()
	body := fmt.Sprintf(`{"query":%q,"limit":50}`, q)
	resp, err := http.Post(base+"/v2/search", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var sr result
	if err := json.NewDecoder(resp.Body).Decode(&sr); err != nil {
		t.Fatal(err)
	}
	sr.Source = resp.Header.Get("X-HC-Source")
	return sr
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func hasPeer(n *testNode, name string, pred func(mesh.Peer) bool) bool {
	for _, p := range n.node.Mesh.Peers() {
		if p.Name == name && pred(p) {
			return true
		}
	}
	return false
}

func TestLocalWarpAndPeersMerge(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// A: public. Brave and DuckDuckGo are blocked on its IP; through WARP
	// Brave works but DuckDuckGo is still blocked.
	a := start(t, ctx, "public-a", true, nil, netKey, engineSpec{per: 2, blocked: map[string]bool{
		"brave": true, "duckduckgo": true, "duckduckgo warp": true,
	}})
	// home: behind NAT (no listener), every engine works, tunnels to A.
	home := start(t, ctx, "home", false, []string{a.url}, netKey, engineSpec{per: 10})
	waitFor(t, "home tunnel on A", func() bool { return hasPeer(a, "home", func(p mesh.Peer) bool { return p.Tunneled }) })

	// B: public, its SearXNG is down entirely; it only knows A.
	b := start(t, ctx, "public-b", true, []string{a.url}, netKey, engineSpec{down: true})
	waitFor(t, "B learning home via A", func() bool { return hasPeer(b, "home", func(p mesh.Peer) bool { return p.Via == a.node.Mesh.ID() }) })

	// 1. A: only 2 local results (google cse), so its blocked engines are
	// scattered: Brave via WARP works, and peers get only the blocked ones.
	r := search(t, a.url, "kerala weather")
	if !r.Success {
		t.Fatalf("A: %s", r.Error)
	}
	for _, want := range []string{"local", "warp", "peer:home"} {
		if !strings.Contains(r.Source, want) {
			t.Fatalf("A: source %q missing %q", r.Source, want)
		}
	}
	for _, want := range []string{"google-cse.example", "brave-warp.example"} {
		if !strings.Contains(r.urls(), want) {
			t.Fatalf("A: results missing %s: %s", want, r.urls())
		}
	}
	homeAsked := home.sx.askedEngines("kerala weather")
	if len(homeAsked) == 0 {
		t.Fatal("home was never asked")
	}
	for _, asked := range homeAsked {
		if asked == nil {
			t.Fatal("home was asked to run every engine; want only the blocked ones")
		}
		for _, e := range asked {
			if e != "brave" && e != "duckduckgo" {
				t.Fatalf("home was asked for %q; only blocked engines should be scattered (%v)", e, asked)
			}
		}
	}
	// A asked its own SearXNG twice: once direct, once for the WARP copies only.
	if got := a.sx.askedEngines("kerala weather"); len(got) != 2 || got[0] != nil || !slices.Equal(got[1], []string{"brave warp", "duckduckgo warp"}) {
		t.Fatalf("A's SearXNG calls = %v, want [all] then [brave warp duckduckgo warp]", got)
	}

	// 2. B's SearXNG is down: every engine is scattered, and the NAT'd home
	// node is reached through A's relay.
	r = search(t, b.url, "malayalam movies")
	if !r.Success || !strings.Contains(r.Source, "peer:home") {
		t.Fatalf("B: want answer including peer:home via relay, got source=%q err=%q", r.Source, r.Error)
	}

	// 3. The same query again on A is served from cache, with no new upstream calls.
	before := len(a.sx.askedEngines("kerala weather")) + len(home.sx.askedEngines("kerala weather"))
	r = search(t, a.url, "  Kerala   WEATHER ")
	if r.Source != "cache" {
		t.Fatalf("repeat query: want cache, got %q", r.Source)
	}
	if after := len(a.sx.askedEngines("kerala weather")) + len(home.sx.askedEngines("kerala weather")); after != before {
		t.Fatalf("cache hit still called SearXNG (%d -> %d)", before, after)
	}

	// 4. A query that is fine locally never touches WARP or peers.
	ok := start(t, ctx, "healthy", true, []string{a.url}, netKey, engineSpec{per: 5})
	r = search(t, ok.url, "onam date")
	if r.Source != "local" {
		t.Fatalf("healthy node: want local only, got %q", r.Source)
	}
	if len(home.sx.askedEngines("onam date")) != 0 || len(ok.sx.askedEngines("onam date")) != 1 {
		t.Fatal("healthy node used other routes for a query it could answer")
	}
}

func TestWrongNetworkKeyCannotJoin(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a := start(t, ctx, "public-a", true, nil, netKey, engineSpec{per: 10})
	intruder := start(t, ctx, "intruder", true, []string{a.url}, "a-completely-different-key", engineSpec{down: true})

	time.Sleep(300 * time.Millisecond)
	if len(a.node.Mesh.Peers()) != 0 {
		t.Fatalf("node with wrong key was admitted: %+v", a.node.Mesh.Peers())
	}
	r := search(t, intruder.url, "anything")
	if strings.Contains(r.Source, "peer:public-a") {
		t.Fatal("intruder got results from the mesh")
	}
}

// A peer's request can only ever run a plain search on this node: the LLM,
// scraping and jobs are reachable only through the node's own API key.
func TestMeshCannotReachLLMOrScraping(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a := start(t, ctx, "public-a", true, nil, netKey, engineSpec{per: 10})
	for _, path := range []string{"/mesh/v1/extract", "/mesh/v1/agent", "/mesh/v1/scrape", "/mesh/v1/crawl"} {
		resp, err := http.Post(a.url+path, "application/json", strings.NewReader(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound && resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("%s answered %d; the mesh must not expose it", path, resp.StatusCode)
		}
	}
}
