// Package node ties the cache, SearXNG, WARP and the mesh together:
//
//	cache → local SearXNG (direct) → enough? done
//	      → engines that were blocked are scattered, in parallel, to
//	        WARP (this node's "<engine> warp" copies) and to peers,
//	        split so each route runs different engines → merge.
package node

import (
	"context"
	"log"
	"strings"
	"sync"
	"time"

	"hivecrawl/internal/cache"
	"hivecrawl/internal/mesh"
	"hivecrawl/internal/searx"
	"hivecrawl/internal/types"
)

const (
	maxPeers       = 3
	localTimeout   = 12 * time.Second
	scatterTimeout = 6 * time.Second
	// warpSuffix names SearXNG's WARP copy of an engine ("brave warp").
	warpSuffix = " warp"
	// staleFor is how old a cached answer may be and still be served when
	// no route can answer (engines blocked everywhere, peers rate limiting).
	staleFor = 48 * time.Hour
)

// warpCategory lists the news and image engines that have WARP copies in
// SearXNG's settings.yml. A blocked category search asks all of them, not
// only the blocked ones: some (yahoo news) are inactive on the direct
// route and only ever run through WARP.
var warpCategory = map[string][]string{
	"news":   {"google news", "brave.news", "duckduckgo news", "yahoo news"},
	"images": {"google images", "brave.images", "duckduckgo images"},
}

type Node struct {
	Name  string
	Mesh  *mesh.Mesh
	cache *cache.Cache
	searx *searx.Client

	minResults int
	// Engines are the direct engines SearXNG runs; used to know what to
	// scatter when local SearXNG fails entirely.
	Engines []string
	// Warp enables retrying blocked engines through their WARP copies.
	Warp bool

	insMu    sync.Mutex
	insights map[string]insight // by cache key; see Insights

	searches rate // searches answered, for the network's pool totals
}

func New(name string, c *cache.Cache, s *searx.Client, minResults int) *Node {
	return &Node{Name: name, cache: c, searx: s, minResults: minResults, insights: map[string]insight{}}
}

// Local answers from cache or this node's SearXNG; it never asks peers.
// It is what peers call over the mesh. req.Engines, when set, restricts
// which engines run.
func (n *Node) Local(ctx context.Context, req types.SearchRequest) (types.SearchResponse, error) {
	start := time.Now()
	req.Normalize()
	key := cache.Key(req)
	if res, _, ok := n.cache.Get(key); ok {
		return respond(res, req.Limit, types.Meta{Source: "cache"}, start), nil
	}
	ctx, cancel := context.WithTimeout(ctx, localTimeout)
	defer cancel()
	out, err := n.searx.Search(ctx, req)
	if err != nil {
		return types.SearchResponse{}, err
	}
	out.Results = applyTimeRange(out.Results, req.TimeRange, time.Now())
	if len(out.Results) >= n.minResults {
		n.cache.Put(key, out.Results, "local")
	}
	n.keepInsights(key, out.Insights)
	return respond(out.Results, req.Limit, types.Meta{Source: "local", Engines: out.Engines, Blocked: out.Blocked}, start), nil
}

// Search is the public entry point.
func (n *Node) Search(ctx context.Context, req types.SearchRequest) types.SearchResponse {
	start := time.Now()
	req.Normalize()
	req.Engines = nil // public searches use every engine
	if req.Category != "" {
		return n.searchCategory(ctx, req, start)
	}
	local, lerr := n.Local(ctx, req)
	if lerr != nil {
		log.Printf("local search failed: %v", lerr)
	}
	if lerr == nil && (local.Meta.Source == "cache" || len(local.Data.Web) >= min(n.minResults, req.Limit)) {
		local.Meta.TookMs = time.Since(start).Milliseconds()
		return local
	}

	blocked := local.Meta.Blocked
	if lerr != nil {
		blocked = n.Engines // SearXNG down: every engine needs another route
	}
	if len(blocked) == 0 {
		// Thin but nothing blocked: the query simply has few results.
		return n.fallback(req, local, lerr, start)
	}

	parts := n.scatter(ctx, req, blocked)
	lists := [][]types.Result{local.Data.Web}
	var sources []string
	engines := local.Meta.Engines
	if len(local.Data.Web) > 0 {
		sources = append(sources, "local")
	}
	for _, p := range parts {
		lists = append(lists, p.results)
		sources = append(sources, p.source)
		engines = union(engines, p.engines)
	}
	if len(parts) == 0 {
		return n.fallback(req, local, lerr, start)
	}
	n.reportAgreement(local.Data.Web, parts)
	merged := merge(lists)
	source := strings.Join(sources, "+")
	if len(merged) >= n.minResults {
		n.cache.Put(cache.Key(req), merged, source)
	}
	return respond(merged, req.Limit, types.Meta{Source: source, Engines: engines, Blocked: blocked}, start)
}

// searchCategory answers image and news searches: this node first, then
// peers that understand categories (these are not split by engine).
func (n *Node) searchCategory(ctx context.Context, req types.SearchRequest, start time.Time) types.SearchResponse {
	local, err := n.Local(ctx, req)
	if err == nil && len(local.Data.Web) == 0 && req.TimeRange != "" {
		// Many news engines skip a query that has a time_range they do not
		// support. Ask without it and enforce the range on publish dates.
		wide := req
		wide.TimeRange, wide.Limit = "", 50
		if r, werr := n.Local(ctx, wide); werr == nil {
			r.Data.Web = applyTimeRange(r.Data.Web, req.TimeRange, time.Now())
			if len(r.Data.Web) > req.Limit {
				r.Data.Web = r.Data.Web[:req.Limit]
			}
			local = r
		}
	}
	if err == nil && len(local.Data.Web) > 0 {
		local.Meta.TookMs = time.Since(start).Milliseconds()
		return local
	}
	// Blocked news/image engines: retry their WARP copies, as web does.
	if n.Warp && err == nil && len(local.Meta.Blocked) > 0 {
		if res := n.warpSearch(ctx, req, union(local.Meta.Blocked, warpCategory[req.Category])); len(res.Results) > 0 {
			n.cache.Put(cache.Key(req), res.Results, "warp")
			return respond(res.Results, req.Limit, types.Meta{Source: "warp", Engines: res.Engines, Blocked: local.Meta.Blocked}, start)
		}
	}
	if !req.LocalOnly && n.Mesh != nil && n.Mesh.Enabled() {
		asked := 0
		for _, p := range n.Mesh.Candidates() {
			if p.Proto < mesh.ProtoCategories { // older peers ignore Category and answer a web search
				continue
			}
			if asked++; asked > maxPeers {
				break
			}
			pctx, cancel := context.WithTimeout(ctx, scatterTimeout)
			pr := req
			pr.Limit = 50
			resp, perr := n.Mesh.Ask(pctx, p, pr)
			cancel()
			if perr != nil {
				log.Printf("peer %s (%s): %v", p.ID, req.Category, perr)
			}
			if perr == nil && len(resp.Data.Web) > 0 {
				n.cache.Put(cache.Key(req), resp.Data.Web, "peer:"+displayName(p))
				return respond(resp.Data.Web, req.Limit, types.Meta{Source: "peer:" + displayName(p), Engines: resp.Meta.Engines}, start)
			}
		}
	}
	return n.fallback(req, local, err, start)
}

// timeRanges are SearXNG's time_range values; tbs=qdr:h maps to "day".
var timeRanges = map[string]time.Duration{
	"day": 24 * time.Hour, "week": 7 * 24 * time.Hour, "month": 31 * 24 * time.Hour, "year": 366 * 24 * time.Hour,
}

// applyTimeRange enforces a time filter on results: engines that report a
// publish date outside the range are dropped, and undated results (engines
// that ignore time_range or give no date) rank after dated ones.
func applyTimeRange(res []types.Result, tr string, now time.Time) []types.Result {
	d, ok := timeRanges[tr]
	if !ok {
		return res
	}
	cutoff := now.Add(-d - 12*time.Hour) // slack for timezones and late indexing
	var dated, undated []types.Result
	for _, r := range res {
		if r.Date == "" {
			undated = append(undated, r)
			continue
		}
		t, err := time.Parse(time.RFC3339, r.Date)
		if err != nil {
			undated = append(undated, r)
			continue
		}
		if t.After(cutoff) {
			dated = append(dated, r)
		}
	}
	out := append(dated, undated...)
	for i := range out {
		out[i].Position = i + 1
	}
	return out
}

// part is what one scatter route returned.
type part struct {
	source  string // "warp" or "peer:<name>"
	peer    string // peer ID, for peer parts
	results []types.Result
	engines []string
}

// scatter runs the blocked engines on other routes at the same time: their
// WARP copies on this node, and a split of them across the best peers.
// Routes that fail or miss the deadline are simply left out.
func (n *Node) scatter(ctx context.Context, req types.SearchRequest, blocked []string) []part {
	ctx, cancel := context.WithTimeout(ctx, scatterTimeout)
	defer cancel()

	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		parts []part
		order = map[string]int{} // keep a stable merge order: warp, then peers by rank
	)
	add := func(rank int, p part) {
		mu.Lock()
		order[p.source] = rank
		parts = append(parts, p)
		mu.Unlock()
	}

	if n.Warp {
		wg.Go(func() {
			out := n.warpSearch(ctx, req, blocked)
			if len(out.Results) == 0 {
				return
			}
			add(0, part{source: "warp", results: out.Results, engines: out.Engines})
		})
	}

	if !req.LocalOnly && n.Mesh != nil && n.Mesh.Enabled() {
		for i, a := range assign(blocked, n.Mesh.Candidates(), maxPeers) {
			wg.Go(func() {
				pr := req
				pr.Limit = 50
				pr.Engines = a.engines
				resp, err := n.Mesh.Ask(ctx, a.peer, pr)
				if err != nil {
					log.Printf("peer %s (%s): %v", a.peer.ID, strings.Join(a.engines, ","), err)
					return
				}
				if len(resp.Data.Web) == 0 {
					return
				}
				add(i+1, part{source: "peer:" + displayName(a.peer), peer: a.peer.ID, results: resp.Data.Web, engines: resp.Meta.Engines})
			})
		}
	}
	wg.Wait()

	// Sort by route rank so merges are deterministic.
	for i := 1; i < len(parts); i++ {
		for j := i; j > 0 && order[parts[j].source] < order[parts[j-1].source]; j-- {
			parts[j], parts[j-1] = parts[j-1], parts[j]
		}
	}
	return parts
}

// warpSearch runs the WARP copies of the given engines. For a time-limited
// category search that comes back empty it asks again without the range
// and filters by publish date, like the direct path does.
func (n *Node) warpSearch(ctx context.Context, req types.SearchRequest, engines []string) searx.Outcome {
	wr := req
	wr.Engines = make([]string, 0, len(engines))
	for _, e := range engines {
		if !strings.HasSuffix(e, warpSuffix) {
			wr.Engines = append(wr.Engines, e+warpSuffix)
		}
	}
	if len(wr.Engines) == 0 {
		return searx.Outcome{}
	}
	out, err := n.searx.Search(ctx, wr)
	if err != nil {
		return searx.Outcome{}
	}
	out.Results = applyTimeRange(out.Results, req.TimeRange, time.Now())
	if len(out.Results) == 0 && req.Category != "" && req.TimeRange != "" {
		wr.TimeRange = ""
		if w, err := n.searx.Search(ctx, wr); err == nil {
			w.Results = applyTimeRange(w.Results, req.TimeRange, time.Now())
			out = w
		}
	}
	return out
}

// ProbeAnswer answers another node's verification probe. It is Local,
// but when this node's direct engines are all blocked it also tries their
// WARP copies: a blocked node is exactly the one that needs the mesh, so
// it must still be able to prove it is a real node.
func (n *Node) ProbeAnswer(ctx context.Context, req types.SearchRequest) (types.SearchResponse, error) {
	resp, err := n.Local(ctx, req)
	if (err == nil && len(resp.Data.Web) > 0) || !n.Warp {
		return resp, err
	}
	start := time.Now()
	out := n.warpSearch(ctx, req, n.Engines)
	if len(out.Results) == 0 {
		return resp, err
	}
	return respond(out.Results, req.Limit, types.Meta{Source: "warp", Engines: out.Engines}, start), nil
}

// displayName is a peer's name for logs and X-HC-Source (IDs are the
// authenticated identity; names are only labels).
func displayName(p mesh.Peer) string {
	if p.Name != "" {
		return p.Name
	}
	return p.ID
}

// agreementTop is how many of a peer's top results are checked.
const agreementTop = 10

// reportAgreement scores each peer's answer by how many of its top results
// some other route (local, WARP or another peer) also returned. Routes run
// different engines, so honest overlap is partial, but a peer returning
// spam or fabricated links matches nothing and its reputation sinks.
func (n *Node) reportAgreement(local []types.Result, parts []part) {
	if n.Mesh == nil {
		return
	}
	for i, p := range parts {
		if p.peer == "" {
			continue
		}
		others := map[string]bool{}
		for _, r := range local {
			others[normalizeURL(r.URL)] = true
		}
		for j, q := range parts {
			if j != i {
				for _, r := range q.results {
					others[normalizeURL(r.URL)] = true
				}
			}
		}
		if len(others) == 0 {
			continue // nothing to compare against
		}
		top := p.results[:min(len(p.results), agreementTop)]
		hit := 0
		for _, r := range top {
			if others[normalizeURL(r.URL)] {
				hit++
			}
		}
		n.Mesh.ReportAgreement(p.peer, float64(hit)/float64(len(top)))
	}
}

type assignment struct {
	peer    mesh.Peer
	engines []string
}

// assign splits engines across up to maxPeers candidates (already sorted
// best first). Each engine goes to the least-loaded peer that has not
// reported it blocked; if every peer reported it blocked (reports can be
// stale), it goes to the least-loaded peer anyway.
func assign(engines []string, cands []mesh.Peer, maxPeers int) []assignment {
	if len(cands) > maxPeers {
		cands = cands[:maxPeers]
	}
	if len(cands) == 0 {
		return nil
	}
	as := make([]assignment, len(cands))
	for i := range cands {
		as[i].peer = cands[i]
	}
	for _, e := range engines {
		best := -1
		for i := range as {
			if contains(as[i].peer.Blocked, e) {
				continue
			}
			if best < 0 || len(as[i].engines) < len(as[best].engines) {
				best = i
			}
		}
		if best < 0 {
			best = 0
			for i := range as {
				if len(as[i].engines) < len(as[best].engines) {
					best = i
				}
			}
		}
		as[best].engines = append(as[best].engines, e)
	}
	out := as[:0]
	for _, a := range as {
		if len(a.engines) > 0 {
			out = append(out, a)
		}
	}
	return out
}

func (n *Node) fallback(req types.SearchRequest, local types.SearchResponse, lerr error, start time.Time) types.SearchResponse {
	if len(local.Data.Web) == 0 {
		// Nothing live answered: an older cached answer beats an empty one.
		if res, src, ok := n.cache.GetStale(cache.Key(req), staleFor); ok {
			return respond(res, req.Limit, types.Meta{Source: "stale:" + src, Blocked: local.Meta.Blocked}, start)
		}
	}
	if len(local.Data.Web) == 0 && len(local.Meta.Blocked) > 0 {
		local.Meta.Retry = true
	}
	if lerr != nil && len(local.Data.Web) == 0 {
		return types.SearchResponse{Success: false, Error: "no results: local search failed and no other route could answer", Data: types.SearchData{Web: []types.Result{}}}
	}
	local.Meta.TookMs = time.Since(start).Milliseconds()
	return local
}

func respond(res []types.Result, limit int, meta types.Meta, start time.Time) types.SearchResponse {
	if len(res) > limit {
		res = res[:limit]
	}
	if res == nil {
		res = []types.Result{}
	}
	meta.TookMs = time.Since(start).Milliseconds()
	return types.SearchResponse{Success: true, Data: types.SearchData{Web: res}, Meta: meta}
}

func union(a, b []string) []string {
	out := append([]string(nil), a...)
	for _, s := range b {
		if !contains(out, s) {
			out = append(out, s)
		}
	}
	return out
}

// BlockedEngines lists this node's direct engines blocked in recent queries
// (shared with peers so they route around them).
func (n *Node) BlockedEngines() []string {
	var out []string
	for e := range n.searx.Status().Blocked {
		if !strings.HasSuffix(e, warpSuffix) {
			out = append(out, e)
		}
	}
	return out
}

// SearxStatus and CacheStats are exposed on /v1/status.
func (n *Node) SearxStatus() searx.Status { return n.searx.Status() }
func (n *Node) CacheStats() cache.Stats   { return n.cache.Stats() }
