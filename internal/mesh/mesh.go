// Package mesh connects HiveCrawl nodes so a node whose engines are blocked
// can ask other nodes to run the search instead.
//
// Public nodes are reached over HTTPS. Nodes behind NAT open tunnels to a
// couple of public nodes; other nodes reach them through those nodes' relay
// endpoint. Peers serve searches from their own cache and SearXNG only: a
// request is never forwarded again, so one query cannot fan out.
//
// Built to stay cheap with thousands of nodes: gossip contacts a random
// sample of peers and shares a capped sample of its table, tunnels spread
// across the least-loaded public nodes, and every request is signed by the
// sender's own key and rate limited per node and per IP.
package mesh

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"hivecrawl/internal/types"
)

// Proto is this node's mesh protocol version, announced in hellos, so
// requests a peer would not understand are never sent to it.
//
//	1: web search
//	2: image and news searches (SearchRequest.Category)
//	3: page fetches for nodes that opt in (HC_SERVE_SCRAPES)
const Proto = 3

// First versions that understand image/news searches and page fetches.
const (
	ProtoCategories = 2
	ProtoScrapes    = 3
)

const (
	gossipFanout   = 8  // nodes greeted per gossip round
	verifyPerRound = 16 // unverified peers probed per gossip round
	sampleDirect   = 48 // public peers shared per hello
	sampleTunneled = 16 // NAT'd peers shared per hello
	maxKnown       = 5000
	peerExpiry     = 3 * time.Minute
	// ipFactor: requests per IP may exceed one node's base rate by this much
	// (several nodes behind one home router), but not by a node per identity.
	ipFactor = 5
	// suspect: peers whose results rarely match any other route are tried last.
	suspectChecks    = 5
	suspectAgreement = 0.15
)

// LocalSearch runs a search on this node only (cache + SearXNG).
type LocalSearch func(ctx context.Context, req types.SearchRequest) (types.SearchResponse, error)

type Options struct {
	Identity  *Identity
	Name      string // display name; IDs come from Identity
	PublicURL string
	Peers     []string // seed node URLs
	Key       string
	// DataDir, if set, keeps a list of public nodes met before, used as
	// extra seeds after a restart.
	DataDir  string
	PeerRate int
	// MaxConcurrent bounds searches served for peers at once (default 4).
	MaxConcurrent int
	// Tunnels a NAT'd node keeps open to public nodes (default 2).
	Tunnels int
	// MaxTunnels a public node accepts (default 1000).
	MaxTunnels  int
	GossipEvery time.Duration // default 30s
	Dialer      *net.Dialer
	Client      *http.Client
	Local       LocalSearch
	// ProbeLocal answers verification probes; defaults to Local. It may
	// use routes Local does not (WARP) so a blocked node can still prove
	// itself.
	ProbeLocal LocalSearch
	// Blocked reports this node's currently blocked engines; it is shared
	// in hellos so peers send those engines elsewhere.
	Blocked func() []string
	// NoIPLimits turns off per-IP limits: only for simulations where every
	// node shares 127.0.0.1.
	NoIPLimits bool
	// ServeScrapes lets peers fetch pages through this node (opt-in);
	// FetchRaw does the fetch (local routes only, never other peers).
	ServeScrapes bool
	FetchRaw     func(ctx context.Context, url string) (*types.RawPage, error)
}

type Peer struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
	URL  string `json:"url,omitempty"`
	// Via is a node holding this peer's tunnel, when it is not us.
	Via string `json:"via,omitempty"`
	// Tunneled means this peer's tunnel terminates on this node.
	Tunneled bool      `json:"tunneled,omitempty"`
	LastSeen time.Time `json:"lastSeen,omitzero"`
	// OnlineSince is the start of the current unbroken stretch of contact.
	OnlineSince time.Time `json:"onlineSince,omitzero"`
	// Priority is the rate multiplier this peer has earned with us.
	Priority float64 `json:"priority"`
	// Blocked lists engines the peer last reported as blocked for its IP.
	Blocked []string `json:"blocked,omitempty"`
	// Proto is the mesh protocol version the peer announced.
	Proto int `json:"proto,omitempty"`
	// Scrapes means the peer fetches pages for others.
	Scrapes bool `json:"scrapes,omitempty"`
	// Tunnels/MaxTunnels are what a public peer reported holding.
	Tunnels    int `json:"tunnels,omitempty"`
	MaxTunnels int `json:"maxTunnels,omitempty"`
	// Verified means the peer answered our probe search recently.
	Verified bool `json:"verified"`
	// Agreement is how often this peer's results matched other routes (EWMA).
	Agreement float64 `json:"agreement"`
	Checks    int     `json:"checks"`

	OK        int   `json:"ok"`
	Fail      int   `json:"fail"`
	Asked     int   `json:"asked"`  // searches we sent to them
	Served    int   `json:"served"` // searches we answered for them
	LatencyMs int64 `json:"latencyMs,omitempty"`
}

// behindNAT reports whether the peer has no public address.
func (p *Peer) behindNAT() bool { return p.URL == "" }

func (p *Peer) suspect() bool { return p.Checks >= suspectChecks && p.Agreement < suspectAgreement }

type Mesh struct {
	id           *Identity
	self         string
	name         string
	publicURL    string
	seeds        []string
	dataDir      string
	cacheMu      sync.Mutex
	key          string
	dialer       *net.Dialer
	client       *http.Client
	local        LocalSearch
	probeLocal   LocalSearch
	probeAnswers *probeStore
	blocked      func() []string
	limit        *limiter
	ipLimit      *limiter
	baseRate     int
	slots        slots
	wantTunnels  int
	maxTunnels   int
	gossipEvery  time.Duration
	noIPLimits   bool
	serveScrape  bool
	fetchRaw     func(ctx context.Context, url string) (*types.RawPage, error)

	mu      sync.Mutex
	peers   map[string]*Peer
	tunnels map[string]*tunnelConn
	served  map[string]int // by requester, including nodes we only see via relays

	verified map[string]time.Time // node -> proof of participation valid until
	probing  map[string]time.Time // node -> last probe attempt
	myRelays []string             // NAT'd node: public nodes holding our tunnels
}

func New(o Options) *Mesh {
	if o.MaxConcurrent == 0 {
		o.MaxConcurrent = 4
	}
	if o.Tunnels == 0 {
		o.Tunnels = 2
	}
	if o.MaxTunnels == 0 {
		o.MaxTunnels = 1000
	}
	if o.GossipEvery == 0 {
		o.GossipEvery = 30 * time.Second
	}
	if o.Identity == nil {
		o.Identity = NewIdentity()
	}
	if o.Name == "" {
		o.Name = o.Identity.ID
	}
	if o.ProbeLocal == nil {
		o.ProbeLocal = o.Local
	}
	return &Mesh{
		id: o.Identity, self: o.Identity.ID, name: o.Name, publicURL: o.PublicURL, seeds: o.Peers, key: o.Key, dataDir: o.DataDir,
		dialer: o.Dialer, client: o.Client, local: o.Local, probeLocal: o.ProbeLocal, blocked: o.Blocked,
		limit: newLimiter(), ipLimit: newLimiter(), baseRate: o.PeerRate, slots: make(slots, max(o.MaxConcurrent, 1)),
		wantTunnels: o.Tunnels, maxTunnels: o.MaxTunnels, gossipEvery: o.GossipEvery, noIPLimits: o.NoIPLimits,
		serveScrape: o.ServeScrapes && o.FetchRaw != nil, fetchRaw: o.FetchRaw,
		peers: map[string]*Peer{}, tunnels: map[string]*tunnelConn{}, served: map[string]int{},
		verified: map[string]time.Time{}, probing: map[string]time.Time{}, probeAnswers: newProbeStore(o.DataDir),
	}
}

func (m *Mesh) Enabled() bool { return m.key != "" }

// ID is this node's identity-derived ID.
func (m *Mesh) ID() string { return m.self }

// Start launches gossip and, for nodes behind NAT, the tunnel manager.
func (m *Mesh) Start(ctx context.Context) {
	if !m.Enabled() {
		return
	}
	m.loadPeerCache()
	go m.refreshProbeAnswers(ctx)
	go func() {
		// Learn public nodes and their load before choosing tunnel relays.
		m.gossipOnce(ctx)
		if m.publicURL == "" {
			go m.tunnelManager(ctx)
		}
		tk := time.NewTicker(m.gossipEvery)
		defer tk.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tk.C:
				m.gossipOnce(ctx)
			}
		}
	}()
}

// Register adds the mesh endpoints to mux.
func (m *Mesh) Register(mux *http.ServeMux) {
	if !m.Enabled() {
		return
	}
	mux.HandleFunc("POST /mesh/v1/hello", m.handleHello)
	mux.HandleFunc("POST /mesh/v1/search", m.handleSearch)
	mux.HandleFunc("POST /mesh/v1/relay/{id}/search", m.handleRelay)
	mux.HandleFunc("POST /mesh/v1/probe", m.handleProbe)
	mux.HandleFunc("POST /mesh/v1/relay/{id}/probe", m.handleRelayProbe)
	mux.HandleFunc("POST /mesh/v1/relay/{id}/scrape", m.handleRelayScrape)
	if m.serveScrape {
		mux.HandleFunc("POST /mesh/v1/scrape", m.handleScrape)
	}
	mux.Handle("GET /mesh/v1/tunnel", m.handleTunnel())
}

// ---- peer table ----

// peer returns the entry for id, creating it unless the table is full
// (nil then). Caller holds mu.
func (m *Mesh) peer(id string) *Peer {
	if p, ok := m.peers[id]; ok {
		return p
	}
	if len(m.peers) >= maxKnown {
		return nil
	}
	p := &Peer{ID: id}
	m.peers[id] = p
	return p
}

// offlineGap is how long without contact resets a peer's uptime.
const offlineGap = 2 * time.Minute

// seen records contact with p, restarting its uptime after a gap. Caller holds mu.
func seen(p *Peer) {
	now := time.Now()
	if p.LastSeen.IsZero() || now.Sub(p.LastSeen) > offlineGap {
		p.OnlineSince = now
	}
	p.LastSeen = now
}

func (m *Mesh) touch(id string) {
	m.mu.Lock()
	if p, ok := m.peers[id]; ok {
		seen(p)
	}
	m.mu.Unlock()
}

// admitTunnel refuses new tunnels past maxTunnels; the home node then
// picks another relay. Spreading load is the home side's job (pickRelay).
func (m *Mesh) admitTunnel(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.tunnels[id]; !ok && len(m.tunnels) >= m.maxTunnels {
		return errors.New("tunnel capacity reached")
	}
	return nil
}

func (m *Mesh) addTunnel(t *tunnelConn, name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if old, ok := m.tunnels[t.peerID]; ok {
		old.ws.Close()
	}
	m.tunnels[t.peerID] = t
	p := m.peer(t.peerID)
	if p == nil {
		return
	}
	p.Tunneled, p.Via, p.URL = true, "", ""
	if name != "" {
		p.Name = name
	}
	seen(p)
}

func (m *Mesh) removeTunnel(t *tunnelConn) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.tunnels[t.peerID] == t {
		delete(m.tunnels, t.peerID)
		if p, ok := m.peers[t.peerID]; ok {
			p.Tunneled = false
		}
	}
}

// TunnelCount is how many NAT'd nodes tunnel to this node.
func (m *Mesh) TunnelCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.tunnels)
}

// Peers returns a snapshot for the status page.
func (m *Mesh) Peers() []Peer {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Peer, 0, len(m.peers))
	for _, p := range m.peers {
		cp := *p
		cp.Served = m.served[p.ID]
		cp.Priority = m.priorityLocked(p.ID)
		cp.Verified = m.verifiedLocked(p.ID)
		out = append(out, cp)
	}
	// Nodes that reached us only through a relay are not in the peer table.
	for id, n := range m.served {
		if _, ok := m.peers[id]; !ok {
			out = append(out, Peer{ID: id, Served: n, Priority: m.priorityLocked(id), Verified: m.verifiedLocked(id)})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// reachable reports whether we currently have a route to p. Caller holds mu.
func (m *Mesh) reachable(p *Peer) bool {
	switch {
	case p.Tunneled:
		return m.tunnels[p.ID] != nil
	case p.URL != "":
		return true
	case p.Via != "":
		r, ok := m.peers[p.Via]
		return ok && r.URL != ""
	}
	return false
}

// Candidates returns peers to ask, best first: NAT'd (home) nodes, which
// engines block less, then by track record. Random jitter spreads load so
// thousands of nodes do not all pick the same few peers. Suspects go last.
func (m *Mesh) Candidates() []Peer {
	m.mu.Lock()
	defer m.mu.Unlock()
	type scored struct {
		p     Peer
		score float64
	}
	var all []scored
	for _, p := range m.peers {
		if !m.reachable(p) {
			continue
		}
		s := 0.7 // unknown track record
		if n := p.OK + p.Fail; n > 0 {
			s = float64(p.OK) / float64(n)
		}
		if p.behindNAT() {
			s += 1
		}
		if p.suspect() {
			s -= 3
		}
		s += rand.Float64() * 0.4
		all = append(all, scored{*p, s})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].score > all[j].score })
	out := make([]Peer, len(all))
	for i, s := range all {
		out[i] = s.p
	}
	return out
}

// ReportAgreement records how much of a peer's answer matched other routes
// for the same query (0..1). Peers that keep returning results nobody else
// sees (spam, poisoned or fake results) sink to the back.
func (m *Mesh) ReportAgreement(id string, agreement float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.peers[id]
	if !ok {
		return
	}
	if p.Checks == 0 {
		p.Agreement = agreement
	} else {
		p.Agreement = 0.7*p.Agreement + 0.3*agreement
	}
	p.Checks++
}

// ---- gossip ----

type helloMsg struct {
	ID         string   `json:"id"`
	Proto      int      `json:"proto,omitempty"`
	Scrapes    bool     `json:"scrapes,omitempty"`
	Name       string   `json:"name,omitempty"`
	URL        string   `json:"url,omitempty"`
	Blocked    []string `json:"blocked,omitempty"`
	Tunnels    int      `json:"tunnels,omitempty"`
	MaxTunnels int      `json:"maxTunnels,omitempty"`
}

func (m *Mesh) selfHello() helloMsg {
	h := helloMsg{ID: m.self, Proto: Proto, Scrapes: m.serveScrape, Name: m.name, URL: m.publicURL}
	if m.blocked != nil {
		h.Blocked = m.blocked()
	}
	if m.publicURL != "" {
		h.Tunnels, h.MaxTunnels = m.TunnelCount(), m.maxTunnels
	}
	return h
}

type helloResp struct {
	Self  helloMsg `json:"self"`
	Peers []Peer   `json:"peers"`
}

// gossipOnce greets a random sample of public nodes (plus seeds while we
// know few), learning peers from each reply.
func (m *Mesh) gossipOnce(ctx context.Context) {
	m.mu.Lock()
	var direct []string
	for _, p := range m.peers {
		if p.URL != "" && p.URL != m.publicURL {
			direct = append(direct, p.URL)
		}
	}
	m.mu.Unlock()
	rand.Shuffle(len(direct), func(i, j int) { direct[i], direct[j] = direct[j], direct[i] })
	if len(direct) > gossipFanout {
		direct = direct[:gossipFanout]
	}
	targets := map[string]bool{}
	for _, u := range direct {
		targets[u] = true
	}
	if len(direct) < gossipFanout {
		for _, s := range m.seeds {
			if s != m.publicURL {
				targets[s] = true
			}
		}
	}

	var wg sync.WaitGroup
	var amu sync.Mutex
	var answered []string
	for u := range targets {
		wg.Go(func() {
			cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			var hr helloResp
			if err := m.post(cctx, u+"/mesh/v1/hello", m.selfHello(), &hr); err == nil {
				m.learn(u, hr)
				amu.Lock()
				answered = append(answered, u)
				amu.Unlock()
			}
		})
	}
	wg.Wait()
	m.expire()
	m.savePeerCache(answered)
	m.verifyPeers()
}

// verifyPeers probes peers that haven't proved themselves yet, so their
// status is known before they ever ask us for a search. maybeVerify keeps
// it cheap: at most one probe per peer per probeRetry, none while verified.
func (m *Mesh) verifyPeers() {
	m.mu.Lock()
	var ids []string
	for id := range m.peers {
		if id != m.self && !m.verifiedLocked(id) {
			ids = append(ids, id)
		}
	}
	for id := range m.tunnels {
		if _, known := m.peers[id]; !known && !m.verifiedLocked(id) {
			ids = append(ids, id)
		}
	}
	m.mu.Unlock()
	rand.Shuffle(len(ids), func(i, j int) { ids[i], ids[j] = ids[j], ids[i] })
	if len(ids) > verifyPerRound {
		ids = ids[:verifyPerRound]
	}
	for _, id := range ids {
		m.maybeVerify(id, route{})
	}
}

// learn merges a hello response from the node at url.
func (m *Mesh) learn(url string, hr helloResp) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if hr.Self.ID == "" || hr.Self.ID == m.self {
		return
	}
	src := m.peer(hr.Self.ID)
	if src == nil {
		return
	}
	src.URL, src.Via, src.Name, src.Proto, src.Scrapes = url, "", hr.Self.Name, hr.Self.Proto, hr.Self.Scrapes
	src.Blocked, src.Tunnels, src.MaxTunnels = hr.Self.Blocked, hr.Self.Tunnels, hr.Self.MaxTunnels
	seen(src)
	for _, p := range hr.Peers {
		if p.ID == "" || p.ID == m.self {
			continue
		}
		known := m.peer(p.ID)
		if known == nil {
			continue
		}
		if known.Tunneled {
			continue // we hold its tunnel ourselves
		}
		if p.Name != "" {
			known.Name = p.Name
		}
		known.Blocked = p.Blocked
		if p.Proto > 0 {
			known.Proto, known.Scrapes = p.Proto, p.Scrapes
		}
		if p.URL != "" {
			if known.URL == "" {
				known.URL = p.URL
			}
			known.Via = ""
			known.Tunnels, known.MaxTunnels = p.Tunnels, p.MaxTunnels
			// Give a newly heard-of node one expiry window to answer our hello.
			if known.LastSeen.IsZero() {
				seen(known)
			}
		} else {
			// Behind NAT and tunneled to the node we just asked.
			known.URL = ""
			known.Via = hr.Self.ID
			seen(known)
		}
	}
}

// expire drops peers we have not heard of recently.
func (m *Mesh) expire() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, p := range m.peers {
		if !p.Tunneled && time.Since(p.LastSeen) > peerExpiry {
			delete(m.peers, id)
		}
	}
	for id, until := range m.verified {
		if time.Now().After(until) {
			delete(m.verified, id)
		}
	}
	for id, at := range m.probing {
		if time.Since(at) > probeRetry {
			delete(m.probing, id)
		}
	}
	if len(m.served) > 2*maxKnown {
		for id := range m.served {
			if _, ok := m.peers[id]; !ok {
				delete(m.served, id)
			}
		}
	}
}

func (m *Mesh) handleHello(w http.ResponseWriter, r *http.Request) {
	if !m.noIPLimits && !m.ipLimit.allow("hello:"+clientIP(r), 120) {
		http.Error(w, "too many hellos", http.StatusTooManyRequests)
		return
	}
	from, body, ok := m.authed(w, r)
	if !ok {
		return
	}
	var h helloMsg
	if err := json.Unmarshal(body, &h); err != nil || h.ID != from {
		http.Error(w, "bad hello", http.StatusBadRequest)
		return
	}
	m.mu.Lock()
	if p := m.peer(h.ID); p != nil && h.ID != m.self {
		if h.URL != "" && !p.Tunneled {
			p.URL, p.Via = h.URL, ""
		}
		p.Name, p.Blocked, p.Tunnels, p.MaxTunnels, p.Proto, p.Scrapes = h.Name, h.Blocked, h.Tunnels, h.MaxTunnels, h.Proto, h.Scrapes
		seen(p)
	}
	var direct, tunneled []Peer
	for _, p := range m.peers {
		if p.ID == from {
			continue
		}
		switch {
		case p.URL != "":
			direct = append(direct, Peer{ID: p.ID, Name: p.Name, URL: p.URL, Blocked: p.Blocked, Tunnels: p.Tunnels, MaxTunnels: p.MaxTunnels, Proto: p.Proto, Scrapes: p.Scrapes})
		case p.Tunneled && m.tunnels[p.ID] != nil:
			tunneled = append(tunneled, Peer{ID: p.ID, Name: p.Name, Blocked: p.Blocked, Proto: p.Proto, Scrapes: p.Scrapes})
		}
	}
	m.mu.Unlock()
	resp := helloResp{Self: m.selfHello(), Peers: append(sample(direct, sampleDirect), sample(tunneled, sampleTunneled)...)}
	writeJSON(w, resp)
}

func sample(ps []Peer, n int) []Peer {
	if len(ps) <= n {
		return ps
	}
	rand.Shuffle(len(ps), func(i, j int) { ps[i], ps[j] = ps[j], ps[i] })
	return ps[:n]
}

// ---- searching ----

// serveSearch answers a search for another node. A node that has not
// proved it serves (see probe.go) gets only a trial allowance while we probe
// it; verified nodes get their earned rate, and contributors go first.
func (m *Mesh) serveSearch(ctx context.Context, from string, hint route, req types.SearchRequest) (types.SearchResponse, error) {
	m.mu.Lock()
	prio := m.priorityLocked(from)
	verified := m.verifiedLocked(from)
	m.mu.Unlock()
	if !verified {
		m.maybeVerify(from, hint)
		if !m.limit.allow("trial:"+from, trialPerMinute) {
			return types.SearchResponse{}, errUnverified
		}
	}
	if !m.limit.allow(from, float64(m.baseRate)*prio) {
		return types.SearchResponse{}, errRateLimited
	}
	if err := m.slots.acquire(ctx, prio >= highPriority); err != nil {
		return types.SearchResponse{}, err
	}
	defer m.slots.release()
	req.Normalize()
	req.LocalOnly = true
	m.mu.Lock()
	m.served[from]++
	m.mu.Unlock()
	return m.local(ctx, req)
}

// priorityLocked is the rate multiplier peer id has earned. Caller holds mu.
func (m *Mesh) priorityLocked(id string) float64 {
	answered, online := 0, time.Duration(0)
	if p, ok := m.peers[id]; ok {
		answered = p.OK
		if !p.OnlineSince.IsZero() && time.Since(p.LastSeen) <= offlineGap {
			online = time.Since(p.OnlineSince)
		}
	}
	return multiplier(answered, m.served[id], m.baseRate, online)
}

var (
	errRateLimited = errors.New("rate limited: too many searches from your node")
	errUnverified  = errors.New("rate limited: unverified node (run a node that answers searches to get full access)")
)

// allowIP caps searches per client IP, so many identities on one machine
// cannot multiply their allowance.
func (m *Mesh) allowIP(r *http.Request) bool {
	if m.noIPLimits {
		return true
	}
	return m.ipLimit.allow("search:"+clientIP(r), float64(m.baseRate*ipFactor))
}

func (m *Mesh) handleSearch(w http.ResponseWriter, r *http.Request) {
	from, body, ok := m.authed(w, r)
	if !ok {
		return
	}
	var req types.SearchRequest
	if err := json.Unmarshal(body, &req); err != nil || req.Query == "" {
		http.Error(w, "bad search request", http.StatusBadRequest)
		return
	}
	if !m.allowIP(r) {
		http.Error(w, "rate limited: too many searches from your IP", http.StatusTooManyRequests)
		return
	}
	resp, err := m.serveSearch(r.Context(), from, routeFrom(r.Header), req)
	if err != nil {
		status := http.StatusBadGateway
		if errors.Is(err, errRateLimited) || errors.Is(err, errBusy) || errors.Is(err, errUnverified) {
			status = http.StatusTooManyRequests
		}
		http.Error(w, err.Error(), status)
		return
	}
	writeJSON(w, resp)
}

// handleRelay forwards a search to a NAT'd node whose tunnel ends here.
func (m *Mesh) handleRelay(w http.ResponseWriter, r *http.Request) {
	from, body, ok := m.authed(w, r)
	if !ok {
		return
	}
	var req types.SearchRequest
	if err := json.Unmarshal(body, &req); err != nil || req.Query == "" {
		http.Error(w, "bad search request", http.StatusBadRequest)
		return
	}
	if !m.allowIP(r) {
		http.Error(w, "rate limited: too many searches from your IP", http.StatusTooManyRequests)
		return
	}
	m.mu.Lock()
	t := m.tunnels[r.PathValue("id")]
	m.mu.Unlock()
	if t == nil {
		http.Error(w, "no tunnel to that node", http.StatusNotFound)
		return
	}
	resp, err := t.call(r.Context(), from, routeFrom(r.Header), req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, resp)
}

// Ask runs req on peer p over whichever route reaches it.
func (m *Mesh) Ask(ctx context.Context, p Peer, req types.SearchRequest) (types.SearchResponse, error) {
	start := time.Now()
	var resp types.SearchResponse
	var err error

	m.mu.Lock()
	t := m.tunnels[p.ID]
	var relayURL string
	if r, ok := m.peers[p.Via]; ok {
		relayURL = r.URL
	}
	m.mu.Unlock()

	switch {
	case p.Tunneled && t != nil:
		resp, err = t.call(ctx, m.self, m.selfRoute(), req)
	case p.URL != "":
		err = m.post(ctx, p.URL+"/mesh/v1/search", req, &resp)
	case relayURL != "":
		err = m.post(ctx, relayURL+"/mesh/v1/relay/"+p.ID+"/search", req, &resp)
	default:
		err = errors.New("no route to peer")
	}

	m.mu.Lock()
	if q, ok := m.peers[p.ID]; ok {
		q.Asked++
		if err != nil {
			q.Fail++
		} else {
			q.OK++
			q.LatencyMs = time.Since(start).Milliseconds()
		}
	}
	m.mu.Unlock()
	return resp, err
}

// ---- HTTP helpers ----

// clientIP is the caller's address. X-Forwarded-For is trusted only from a
// local reverse proxy (Caddy/nginx on the same host or Docker network).
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	if ip != nil && (ip.IsLoopback() || ip.IsPrivate()) {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			first, _, _ := bytes.Cut([]byte(xff), []byte(","))
			if f := net.ParseIP(string(bytes.TrimSpace(first))); f != nil {
				return f.String()
			}
		}
	}
	return host
}

func (m *Mesh) authed(w http.ResponseWriter, r *http.Request) (string, []byte, bool) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 64<<10))
	if err != nil {
		http.Error(w, "read error", http.StatusBadRequest)
		return "", nil, false
	}
	from, err := verify(m.key, r, body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return "", nil, false
	}
	return from, body, true
}

func (m *Mesh) post(ctx context.Context, url string, in, out any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return err
	}
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	r.Header.Set("Content-Type", "application/json")
	signHeaders(r.Header, m.id, m.key, http.MethodPost, r.URL.Path, body)
	sr := m.selfRoute()
	if sr.URL != "" {
		r.Header.Set(hdrURL, sr.URL)
	}
	if len(sr.Relays) > 0 {
		r.Header.Set(hdrRelays, strings.Join(sr.Relays, ","))
	}
	resp, err := m.client.Do(r)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return fmt.Errorf("%s: status %d: %s", url, resp.StatusCode, bytes.TrimSpace(b))
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(out)
}

// selfRoute tells peers how to reach us for probes.
func (m *Mesh) selfRoute() route {
	m.mu.Lock()
	defer m.mu.Unlock()
	return route{URL: m.publicURL, Relays: append([]string(nil), m.myRelays...)}
}

func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("write json: %v", err)
	}
}
