// Package sim load-tests the mesh in one process: a few public nodes and
// many residential (NAT'd, listener-less) nodes. Run with:
//
//	HC_SIM=1 HC_SIM_HOMES=1000 go test -run TestSimulateNetwork -v ./internal/sim/
//
// Every home node is seeded with only the first public node (worst case);
// the test checks that tunnels spread out, gossip traffic stays bounded,
// and searches from every kind of node succeed.
package sim

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"slices"
	"strconv"
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
	"hivecrawl/internal/types"
)

const netKey = "sim-network-key-0123456789"

var engines = []string{"google cse", "brave", "duckduckgo", "bing"}

// fakeSearx serves every node at /n/<i>/search. Public nodes (i < publics)
// have most engines blocked; home nodes have one random engine blocked.
type fakeSearx struct {
	publics int
	blocked sync.Map // node index -> map[string]bool
}

func (f *fakeSearx) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(r.URL.Path, "/") // "", "n", "<i>", "search"
	i, _ := strconv.Atoi(parts[2])
	bl, _ := f.blocked.Load(i)
	blocked, _ := bl.(map[string]bool)
	want := engines
	if e := r.URL.Query().Get("engines"); e != "" {
		want = strings.Split(e, ",")
	}
	time.Sleep(time.Duration(20+rand.IntN(60)) * time.Millisecond) // upstream latency
	q := strings.ReplaceAll(r.URL.Query().Get("q"), " ", "-")
	var res []map[string]any
	var unresp [][]string
	for _, e := range want {
		if blocked[e] || strings.HasSuffix(e, " warp") {
			unresp = append(unresp, []string{e, "CAPTCHA"})
			continue
		}
		for k := range 5 {
			res = append(res, map[string]any{
				"url": fmt.Sprintf("https://%s.example/%s/%d", strings.ReplaceAll(e, " ", "-"), q, k), "title": e, "content": q, "engines": []string{e},
			})
		}
	}
	json.NewEncoder(w).Encode(map[string]any{"results": res, "unresponsive_engines": unresp})
}

type counter struct {
	reqs, bytes atomic.Int64
}

type countingWriter struct {
	http.ResponseWriter
	c *counter
}

func (w countingWriter) Write(b []byte) (int, error) {
	w.c.bytes.Add(int64(len(b)))
	return w.ResponseWriter.Write(b)
}

func envInt(k string, def int) int {
	if n, err := strconv.Atoi(os.Getenv(k)); err == nil {
		return n
	}
	return def
}

func TestSimulateNetwork(t *testing.T) {
	if os.Getenv("HC_SIM") == "" {
		t.Skip("set HC_SIM=1 to run the network simulation")
	}
	publics, homes := envInt("HC_SIM_PUBLICS", 5), envInt("HC_SIM_HOMES", 300)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	fs := &fakeSearx{publics: publics}
	sx := httptest.NewServer(fs)
	defer sx.Close()
	var hello counter

	var before runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	goBefore := runtime.NumGoroutine()

	dialer := netx.PublicDialer(nil)
	client := netx.Client(dialer, 10*time.Second)
	client.Transport.(*http.Transport).MaxIdleConnsPerHost = 64

	type simNode struct {
		n   *node.Node
		url string
	}
	newNode := func(i int, public bool, seeds []string) simNode {
		if i < publics {
			fs.blocked.Store(i, map[string]bool{"brave": true, "duckduckgo": true, "bing": true})
		} else {
			fs.blocked.Store(i, map[string]bool{engines[rand.IntN(len(engines))]: true})
		}
		n := node.New(fmt.Sprintf("n%d", i), cache.New(time.Minute, 1000), searx.New(fmt.Sprintf("%s/n/%d", sx.URL, i), client), 8)
		n.Engines, n.Warp = engines, true
		sn := simNode{n: n}
		var handler http.Handler
		if public {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/mesh/v1/hello" {
					hello.reqs.Add(1)
					w = countingWriter{w, &hello}
				}
				handler.ServeHTTP(w, r)
			}))
			t.Cleanup(srv.Close)
			sn.url = srv.URL
		}
		n.Mesh = mesh.New(mesh.Options{
			Name: n.Name, PublicURL: sn.url, Peers: seeds, Key: netKey, PeerRate: 1000,
			GossipEvery: 2 * time.Second, Dialer: dialer, Client: client, Local: n.Local, Blocked: n.BlockedEngines,
			NoIPLimits: true,
		})
		mux := http.NewServeMux()
		(&api.Server{Node: n, PublicURL: sn.url, Started: time.Now()}).Register(mux)
		n.Mesh.Register(mux)
		handler = mux
		n.Mesh.Start(ctx)
		return sn
	}

	pubs := make([]simNode, publics)
	for i := range publics {
		var seeds []string
		if i > 0 {
			seeds = []string{pubs[0].url}
		}
		pubs[i] = newNode(i, true, seeds)
	}
	time.Sleep(3 * time.Second) // publics find each other

	start := time.Now()
	hs := make([]simNode, homes)
	for i := range homes {
		hs[i] = newNode(publics+i, false, []string{pubs[0].url}) // all seeded with ONE public
		if i%50 == 49 {
			time.Sleep(200 * time.Millisecond) // homes join in waves
		}
	}

	// Wait for every home to hold 2 tunnels.
	deadline := time.Now().Add(90 * time.Second)
	var total int
	for time.Now().Before(deadline) {
		total = 0
		for _, p := range pubs {
			total += p.n.Mesh.TunnelCount()
		}
		if total >= 2*homes {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	joined := time.Since(start)
	var counts []int
	for _, p := range pubs {
		counts = append(counts, p.n.Mesh.TunnelCount())
	}
	t.Logf("tunnels: %d/%d established in %s; per public node: %v", total, 2*homes, joined.Round(time.Millisecond), counts)
	if total < 2*homes*95/100 {
		t.Errorf("only %d of %d tunnels established", total, 2*homes)
	}
	if mx, mn := slices.Max(counts), slices.Min(counts); mx > 3*max(mn, 1)+20 {
		t.Errorf("tunnels not spread across public nodes: %v", counts)
	}

	// Gossip cost over a 10s window, steady state.
	hello.reqs.Store(0)
	hello.bytes.Store(0)
	time.Sleep(10 * time.Second)
	hr, hb := hello.reqs.Load(), hello.bytes.Load()
	// The simulation gossips every 2s; production every 30s (15x less).
	t.Logf("gossip: %.1f hellos per node per round; at the production 30s interval: %.0f KB/s total, %.0f KB/s per public node, %.1f KB per hello",
		float64(hr)/5/float64(publics+homes), float64(hb)/10/1024/15, float64(hb)/10/1024/15/float64(publics), float64(hb)/float64(max(hr, 1))/1024)

	// Peer table sizes stay bounded.
	maxPeers := 0
	for _, s := range append(pubs, hs...) {
		maxPeers = max(maxPeers, len(s.n.Mesh.Peers()))
	}
	t.Logf("largest peer table: %d entries", maxPeers)

	// Searches from random nodes (public nodes are thin locally, so they
	// scatter to peers; homes mostly answer locally).
	var lat []time.Duration
	var mu sync.Mutex
	var ok, viaPeer atomic.Int32
	queries := envInt("HC_SIM_QUERIES", 400)
	var wg sync.WaitGroup
	sem := make(chan struct{}, 50)
	for q := range queries {
		var sn simNode
		if q%2 == 0 {
			sn = pubs[q%publics]
		} else {
			sn = hs[rand.IntN(homes)]
		}
		sem <- struct{}{}
		wg.Go(func() {
			defer func() { <-sem }()
			t0 := time.Now()
			resp := sn.n.Search(ctx, typesReq(fmt.Sprintf("query %d", q)))
			d := time.Since(t0)
			if resp.Success && len(resp.Data.Web) > 0 {
				ok.Add(1)
			}
			if strings.Contains(resp.Meta.Source, "peer:") {
				viaPeer.Add(1)
			}
			mu.Lock()
			lat = append(lat, d)
			mu.Unlock()
		})
	}
	wg.Wait()
	slices.Sort(lat)
	pct := func(p float64) time.Duration { return lat[int(float64(len(lat)-1)*p)].Round(time.Millisecond) }
	t.Logf("searches: %d/%d succeeded, %d used peers; latency p50 %s p95 %s p99 %s",
		ok.Load(), queries, viaPeer.Load(), pct(0.5), pct(0.95), pct(0.99))
	if int(ok.Load()) < queries*99/100 {
		t.Errorf("only %d/%d searches succeeded", ok.Load(), queries)
	}

	var after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&after)
	nodes := publics + homes
	t.Logf("memory: %.1f MB heap for %d nodes (%.0f KB per node incl. test servers), goroutines %d",
		float64(after.HeapInuse-before.HeapInuse)/1e6, nodes, float64(after.HeapInuse-before.HeapInuse)/1e3/float64(nodes), runtime.NumGoroutine()-goBefore)
}

func typesReq(q string) types.SearchRequest { return types.SearchRequest{Query: q, Limit: 10} }
