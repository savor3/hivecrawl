package mesh

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"hivecrawl/internal/types"
)

// Page fetches through peers, for nodes that opt in (HC_SERVE_SCRAPES).
// A peer only downloads the raw page with its own routes (direct and its
// WARP) — no browser, no LLM — after its internal-address check, and the
// asking node converts it. Same gates as search: verified nodes, per-node
// and per-IP limits.

const maxPageBytes = 5 << 20

var errNoScrapes = errors.New("this node does not fetch pages for peers")

type scrapeReq struct {
	URL string `json:"url"`
}

func (m *Mesh) serveScrapeReq(ctx context.Context, from string, hint route, u string) (*types.RawPage, error) {
	if !m.serveScrape {
		return nil, errNoScrapes
	}
	if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
		return nil, errors.New("url must be http(s)")
	}
	m.mu.Lock()
	verified := m.verifiedLocked(from)
	m.mu.Unlock()
	if !verified {
		m.maybeVerify(from, hint)
		if !m.limit.allow("trial:"+from, trialPerMinute) {
			return nil, errUnverified
		}
	}
	// Page fetches are heavier than searches: a third of the search rate.
	if !m.limit.allow("scrape:"+from, float64(max(m.baseRate/3, 1))) {
		return nil, errRateLimited
	}
	if err := m.slots.acquire(ctx, false); err != nil {
		return nil, err
	}
	defer m.slots.release()
	page, err := m.fetchRaw(ctx, u)
	if err != nil {
		return nil, err
	}
	if len(page.Body) > maxPageBytes {
		page.Body = page.Body[:maxPageBytes]
	}
	return page, nil
}

func (m *Mesh) handleScrape(w http.ResponseWriter, r *http.Request) {
	from, body, ok := m.authed(w, r)
	if !ok {
		return
	}
	var req scrapeReq
	if err := json.Unmarshal(body, &req); err != nil || req.URL == "" {
		http.Error(w, "bad scrape request", http.StatusBadRequest)
		return
	}
	if !m.allowIP(r) {
		http.Error(w, "rate limited: too many requests from your IP", http.StatusTooManyRequests)
		return
	}
	page, err := m.serveScrapeReq(r.Context(), from, routeFrom(r.Header), req.URL)
	if err != nil {
		status := http.StatusBadGateway
		if errors.Is(err, errRateLimited) || errors.Is(err, errBusy) || errors.Is(err, errUnverified) {
			status = http.StatusTooManyRequests
		}
		http.Error(w, err.Error(), status)
		return
	}
	writeJSON(w, page)
}

func (m *Mesh) handleRelayScrape(w http.ResponseWriter, r *http.Request) {
	from, body, ok := m.authed(w, r)
	if !ok {
		return
	}
	var req scrapeReq
	if err := json.Unmarshal(body, &req); err != nil || req.URL == "" {
		http.Error(w, "bad scrape request", http.StatusBadRequest)
		return
	}
	if !m.allowIP(r) {
		http.Error(w, "rate limited: too many requests from your IP", http.StatusTooManyRequests)
		return
	}
	m.mu.Lock()
	t := m.tunnels[r.PathValue("id")]
	m.mu.Unlock()
	if t == nil {
		http.Error(w, "no tunnel to that node", http.StatusNotFound)
		return
	}
	page, err := t.scrape(r.Context(), from, routeFrom(r.Header), req.URL)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, page)
}

// FetchViaPeers asks up to two peers that fetch pages for others. It
// returns the page and the peer's name.
func (m *Mesh) FetchViaPeers(ctx context.Context, u string) (*types.RawPage, string, error) {
	tried := 0
	lastErr := errors.New("no peer fetches pages for others")
	for _, p := range m.Candidates() {
		if !p.Scrapes || p.Proto < ProtoScrapes {
			continue
		}
		if tried++; tried > 2 {
			break
		}
		m.mu.Lock()
		t := m.tunnels[p.ID]
		var relayURL string
		if r, ok := m.peers[p.Via]; ok {
			relayURL = r.URL
		}
		m.mu.Unlock()
		var page types.RawPage
		var err error
		switch {
		case p.Tunneled && t != nil:
			var pp *types.RawPage
			if pp, err = t.scrape(ctx, m.self, m.selfRoute(), u); err == nil {
				page = *pp
			}
		case p.URL != "":
			err = m.post(ctx, p.URL+"/mesh/v1/scrape", scrapeReq{URL: u}, &page)
		case relayURL != "":
			err = m.post(ctx, relayURL+"/mesh/v1/relay/"+p.ID+"/scrape", scrapeReq{URL: u}, &page)
		default:
			continue
		}
		if err == nil {
			name := p.Name
			if name == "" {
				name = p.ID
			}
			return &page, name, nil
		}
		lastErr = err
	}
	return nil, "", lastErr
}
