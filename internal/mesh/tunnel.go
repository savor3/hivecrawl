package mesh

import (
	"context"
	"errors"
	"log"
	"math/rand/v2"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/net/websocket"

	"hivecrawl/internal/types"
)

// A tunnel lets a node behind NAT (a home router) serve searches. The home
// node dials out to a public node over a websocket; the public node then
// sends search frames down that connection. Only search requests travel
// through the tunnel, so it is never a general-purpose proxy.

type frame struct {
	ID   uint64 `json:"id"`
	Kind string `json:"kind"` // search | probe | result | ping | pong
	From string `json:"from,omitempty"`
	// How to reach From, so the home node can probe it (see probe.go).
	FromURL    string                `json:"fromUrl,omitempty"`
	FromRelays []string              `json:"fromRelays,omitempty"`
	Req        *types.SearchRequest  `json:"req,omitempty"`
	Resp       *types.SearchResponse `json:"resp,omitempty"`
	Probe      *probeReq             `json:"probe,omitempty"`
	ProbeResp  *probeResp            `json:"probeResp,omitempty"`
	ScrapeURL  string                `json:"scrapeUrl,omitempty"`
	Page       *types.RawPage        `json:"page,omitempty"`
	Err        string                `json:"err,omitempty"`
}

// Both ends ping. Home connections often die silently (Wi-Fi or router NAT
// drops the flow with no FIN), so the home side gives up on a quiet tunnel
// quickly and redials.
const (
	pingEvery       = 20 * time.Second
	readTimeout     = 60 * time.Second // public side
	homeReadTimeout = 45 * time.Second
)

// tunnelConn is the public node's end of a home node's tunnel.
type tunnelConn struct {
	peerID string
	ws     *websocket.Conn
	sendMu sync.Mutex
	nextID atomic.Uint64

	mu      sync.Mutex
	pending map[uint64]chan frame
	closed  chan struct{}
}

func (t *tunnelConn) send(f frame) error {
	t.sendMu.Lock()
	defer t.sendMu.Unlock()
	t.ws.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return websocket.JSON.Send(t.ws, f)
}

// roundTrip sends f down the tunnel and waits for its result frame.
func (t *tunnelConn) roundTrip(ctx context.Context, f frame) (frame, error) {
	f.ID = t.nextID.Add(1)
	ch := make(chan frame, 1)
	t.mu.Lock()
	t.pending[f.ID] = ch
	t.mu.Unlock()
	defer func() {
		t.mu.Lock()
		delete(t.pending, f.ID)
		t.mu.Unlock()
	}()
	if err := t.send(f); err != nil {
		return frame{}, err
	}
	select {
	case r := <-ch:
		if r.Err != "" {
			return frame{}, errors.New(r.Err)
		}
		return r, nil
	case <-t.closed:
		return frame{}, errors.New("tunnel closed")
	case <-ctx.Done():
		return frame{}, ctx.Err()
	}
}

// call sends a search down the tunnel on behalf of from.
func (t *tunnelConn) call(ctx context.Context, from string, hint route, req types.SearchRequest) (types.SearchResponse, error) {
	r, err := t.roundTrip(ctx, frame{Kind: "search", From: from, FromURL: hint.URL, FromRelays: hint.Relays, Req: &req})
	if err != nil {
		return types.SearchResponse{}, err
	}
	if r.Resp == nil {
		return types.SearchResponse{}, errors.New("empty tunnel response")
	}
	return *r.Resp, nil
}

// scrape asks the home node to fetch a page on behalf of from.
func (t *tunnelConn) scrape(ctx context.Context, from string, hint route, u string) (*types.RawPage, error) {
	r, err := t.roundTrip(ctx, frame{Kind: "scrape", From: from, FromURL: hint.URL, FromRelays: hint.Relays, ScrapeURL: u})
	if err != nil {
		return nil, err
	}
	if r.Page == nil {
		return nil, errors.New("empty scrape response")
	}
	return r.Page, nil
}

// probe asks the home node to answer a probe search.
func (t *tunnelConn) probe(ctx context.Context, pr probeReq) (probeResp, error) {
	r, err := t.roundTrip(ctx, frame{Kind: "probe", Probe: &pr})
	if err != nil {
		return probeResp{}, err
	}
	if r.ProbeResp == nil {
		return probeResp{}, errors.New("empty probe response")
	}
	return *r.ProbeResp, nil
}

// handleTunnel accepts a home node's tunnel (public-node side).
func (m *Mesh) handleTunnel() http.Handler {
	return websocket.Server{
		Handshake: func(_ *websocket.Config, r *http.Request) error {
			id, err := verify(m.key, r, nil)
			if err != nil {
				return err
			}
			return m.admitTunnel(id)
		},
		Handler: func(ws *websocket.Conn) {
			// verify() in Handshake authenticated this header.
			id := ws.Request().Header.Get(hdrNode)
			t := &tunnelConn{peerID: id, ws: ws, pending: map[uint64]chan frame{}, closed: make(chan struct{})}
			m.addTunnel(t, ws.Request().Header.Get(hdrName))
			defer func() {
				close(t.closed)
				m.removeTunnel(t)
				ws.Close()
			}()

			stop := make(chan struct{})
			defer close(stop)
			go func() {
				tk := time.NewTicker(pingEvery)
				defer tk.Stop()
				for {
					select {
					case <-stop:
						return
					case <-tk.C:
						if t.send(frame{Kind: "ping"}) != nil {
							ws.Close()
							return
						}
					}
				}
			}()

			for {
				ws.SetReadDeadline(time.Now().Add(readTimeout))
				var f frame
				if err := websocket.JSON.Receive(ws, &f); err != nil {
					return
				}
				switch f.Kind {
				case "result":
					t.mu.Lock()
					ch := t.pending[f.ID]
					t.mu.Unlock()
					if ch != nil {
						ch <- f
					}
				case "ping":
					m.touch(id)
					if t.send(frame{Kind: "pong"}) != nil {
						return
					}
				case "pong":
					m.touch(id)
				}
			}
		},
	}
}

// hdrName carries the home node's display name on the tunnel handshake.
const hdrName = "X-HC-Name"

// tunnelManager keeps wantTunnels tunnels open from a NAT'd node, to the
// least-loaded public nodes it knows (seeds until gossip finds others).
// A relay that refuses or drops us is avoided for a while.
func (m *Mesh) tunnelManager(ctx context.Context) {
	active := map[string]bool{}
	avoid := map[string]time.Time{}
	done := make(chan string)
	tk := time.NewTicker(5 * time.Second)
	defer tk.Stop()
	for {
		m.setRelays(active)
		for len(active) < m.wantTunnels {
			u := m.pickRelay(active, avoid)
			if u == "" {
				break
			}
			active[u] = true
			go func() {
				start := time.Now()
				err := m.serveTunnel(ctx, u)
				if ctx.Err() == nil && time.Since(start) > time.Minute {
					err = nil // ran fine for a while; reconnect promptly
				}
				if err != nil {
					log.Printf("tunnel to %s: %v", u, err)
				}
				select {
				case done <- u:
				case <-ctx.Done():
				}
			}()
		}
		select {
		case <-ctx.Done():
			return
		case u := <-done:
			delete(active, u)
			avoid[u] = time.Now()
		case <-tk.C:
		}
	}
}

// setRelays records which public nodes hold our tunnels (sent to peers so
// they can probe us).
func (m *Mesh) setRelays(active map[string]bool) {
	rs := make([]string, 0, len(active))
	for u := range active {
		rs = append(rs, u)
	}
	m.mu.Lock()
	m.myRelays = rs
	m.mu.Unlock()
}

// relayAvoid is how long a relay that refused or dropped us is skipped.
const relayAvoid = 15 * time.Second

// pickRelay chooses a public node not already used, by "power of two
// choices": take two at random and keep the less loaded. Load figures come
// from gossip and are seconds old; always taking the single least-loaded
// node would send every joining home node to the same relay.
func (m *Mesh) pickRelay(active map[string]bool, avoid map[string]time.Time) string {
	type cand struct {
		url  string
		load float64
	}
	var cs []cand
	ok := func(u string) bool {
		t, bad := avoid[u]
		return u != "" && u != m.publicURL && !active[u] && (!bad || time.Since(t) > relayAvoid)
	}
	seen := map[string]bool{}
	m.mu.Lock()
	for _, p := range m.peers {
		if p.URL != "" && ok(p.URL) && !seen[p.URL] {
			seen[p.URL] = true
			load := float64(p.Tunnels)
			if p.MaxTunnels > 0 {
				load = float64(p.Tunnels) / float64(p.MaxTunnels)
			}
			cs = append(cs, cand{p.URL, load})
		}
	}
	m.mu.Unlock()
	for _, s := range m.seeds {
		if ok(s) && !seen[s] {
			seen[s] = true
			cs = append(cs, cand{s, 0.5}) // unknown load
		}
	}
	switch len(cs) {
	case 0:
		return ""
	case 1:
		return cs[0].url
	}
	a, b := cs[rand.IntN(len(cs))], cs[rand.IntN(len(cs))]
	if b.load < a.load {
		a = b
	}
	return a.url
}

// serveTunnel runs one tunnel session to a public node (home-node side)
// until it fails or ctx ends.
func (m *Mesh) serveTunnel(ctx context.Context, peerURL string) error {
	wsURL := "ws" + strings.TrimPrefix(peerURL, "http") + "/mesh/v1/tunnel"
	cfg, err := websocket.NewConfig(wsURL, peerURL)
	if err != nil {
		return err
	}
	cfg.Dialer = m.dialer
	signHeaders(cfg.Header, m.id, m.key, http.MethodGet, "/mesh/v1/tunnel", nil)
	cfg.Header.Set(hdrName, m.name)
	ws, err := cfg.DialContext(ctx)
	if err != nil {
		return err
	}
	defer ws.Close()
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			ws.Close()
		case <-done:
		}
	}()

	var sendMu sync.Mutex
	send := func(f frame) error {
		sendMu.Lock()
		defer sendMu.Unlock()
		ws.SetWriteDeadline(time.Now().Add(10 * time.Second))
		return websocket.JSON.Send(ws, f)
	}
	go func() {
		tk := time.NewTicker(pingEvery)
		defer tk.Stop()
		for {
			select {
			case <-done:
				return
			case <-tk.C:
				if send(frame{Kind: "ping"}) != nil {
					ws.Close()
					return
				}
			}
		}
	}()
	for {
		ws.SetReadDeadline(time.Now().Add(homeReadTimeout))
		var f frame
		if err := websocket.JSON.Receive(ws, &f); err != nil {
			return err
		}
		switch f.Kind {
		case "ping":
			send(frame{Kind: "pong"})
		case "probe":
			if f.Probe == nil {
				continue
			}
			go func(f frame) {
				resp, err := m.answerProbe(ctx, *f.Probe)
				out := frame{ID: f.ID, Kind: "result"}
				if err != nil {
					out.Err = err.Error()
				} else {
					out.ProbeResp = &resp
				}
				send(out)
			}(f)
		case "scrape":
			go func(f frame) {
				page, err := m.serveScrapeReq(ctx, f.From, route{URL: f.FromURL, Relays: f.FromRelays}, f.ScrapeURL)
				out := frame{ID: f.ID, Kind: "result"}
				if err != nil {
					out.Err = err.Error()
				} else {
					out.Page = page
				}
				send(out)
			}(f)
		case "search":
			if f.Req == nil {
				continue
			}
			go func(f frame) {
				resp, err := m.serveSearch(ctx, f.From, route{URL: f.FromURL, Relays: f.FromRelays}, *f.Req)
				out := frame{ID: f.ID, Kind: "result"}
				if err != nil {
					out.Err = err.Error()
				} else {
					out.Resp = &resp
				}
				send(out)
			}(f)
		}
	}
}
