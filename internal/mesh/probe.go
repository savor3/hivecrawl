package mesh

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	mrand "math/rand/v2"
	"net/http"
	"strings"
	"time"
)

// Proof of participation: the mesh only serves nodes that serve.
//
// A node asking for searches must itself answer a probe search before it
// gets more than a small trial allowance. The probe goes back to it over the
// mesh (its public URL, its tunnel, or a relay holding its tunnel); the
// answer is signed with its key and must share results with the prober's own
// search for the same query. A script that only takes cannot answer, so it
// stays at the trial rate however many identities it creates.
const (
	verifyFor      = time.Hour
	probeRetry     = 5 * time.Minute
	trialPerMinute = 3
	probeTimeout   = 15 * time.Second
	hdrURL         = "X-HC-URL"    // sender's public URL, if any
	hdrRelays      = "X-HC-Relays" // public nodes holding the sender's tunnels
)

// Cached everywhere after first use, so probes cost a node almost nothing.
var probeQueries = []string{
	"weather forecast", "world news", "wikipedia", "python tutorial", "football scores",
	"currency exchange rates", "easy recipes", "linux commands", "time zones", "public holidays",
}

type probeReq struct {
	Nonce string `json:"nonce"`
	Query string `json:"query"`
}

type probeResp struct {
	Nonce string   `json:"nonce"`
	URLs  []string `json:"urls"`
	Pub   string   `json:"pub"`
	Sig   string   `json:"sig"`
}

// route is how a requester said it can be reached (unauthenticated hints;
// the signed probe answer is what proves identity).
type route struct {
	URL    string
	Relays []string
}

func routeFrom(h http.Header) route {
	r := route{URL: h.Get(hdrURL)}
	for _, u := range strings.Split(h.Get(hdrRelays), ",") {
		if u = strings.TrimSpace(u); u != "" && len(r.Relays) < 4 {
			r.Relays = append(r.Relays, u)
		}
	}
	return r
}

func probeMessage(nonce, query string, urls []string) []byte {
	return []byte("hivecrawl-probe/1\n" + nonce + "\n" + query + "\n" + strings.Join(urls, "\n"))
}

// answerProbe runs the probe query locally and signs the answer.
func (m *Mesh) answerProbe(ctx context.Context, pr probeReq) (probeResp, error) {
	urls := m.probeSearch(ctx, pr.Query)
	if len(urls) == 0 {
		return probeResp{}, errors.New("no results for probe")
	}
	return probeResp{
		Nonce: pr.Nonce, URLs: urls,
		Pub: base64.StdEncoding.EncodeToString(m.id.Pub),
		Sig: base64.StdEncoding.EncodeToString(ed25519.Sign(m.id.priv, probeMessage(pr.Nonce, pr.Query, urls))),
	}, nil
}

func (m *Mesh) handleProbe(w http.ResponseWriter, r *http.Request) {
	from, body, ok := m.authed(w, r)
	if !ok {
		return
	}
	if !m.limit.allow("probe:"+from, 10) {
		http.Error(w, "too many probes", http.StatusTooManyRequests)
		return
	}
	pr, ok := decodeProbe(w, body)
	if !ok {
		return
	}
	resp, err := m.answerProbe(r.Context(), pr)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, resp)
}

func (m *Mesh) handleRelayProbe(w http.ResponseWriter, r *http.Request) {
	_, body, ok := m.authed(w, r)
	if !ok {
		return
	}
	pr, ok := decodeProbe(w, body)
	if !ok {
		return
	}
	m.mu.Lock()
	t := m.tunnels[r.PathValue("id")]
	m.mu.Unlock()
	if t == nil {
		http.Error(w, "no tunnel to that node", http.StatusNotFound)
		return
	}
	resp, err := t.probe(r.Context(), pr)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, resp)
}

func decodeProbe(w http.ResponseWriter, body []byte) (probeReq, bool) {
	var pr probeReq
	if err := jsonUnmarshal(body, &pr); err != nil || pr.Nonce == "" || pr.Query == "" || len(pr.Query) > 100 {
		http.Error(w, "bad probe", http.StatusBadRequest)
		return pr, false
	}
	return pr, true
}

// verified reports whether id proved itself recently. Caller holds mu.
func (m *Mesh) verifiedLocked(id string) bool {
	return time.Now().Before(m.verified[id])
}

// maybeVerify starts a background probe of id, at most once per probeRetry.
func (m *Mesh) maybeVerify(id string, hint route) {
	m.mu.Lock()
	if m.verifiedLocked(id) || time.Since(m.probing[id]) < probeRetry {
		m.mu.Unlock()
		return
	}
	m.probing[id] = time.Now()
	m.mu.Unlock()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
		defer cancel()
		if err := m.probe(ctx, id, hint); err != nil {
			return
		}
		m.mu.Lock()
		m.verified[id] = time.Now().Add(verifyFor)
		m.mu.Unlock()
	}()
}

// probe sends a probe to id over every route we know until one answers
// correctly.
func (m *Mesh) probe(ctx context.Context, id string, hint route) error {
	var nb [12]byte
	rand.Read(nb[:])
	pr := probeReq{Nonce: hex.EncodeToString(nb[:]), Query: probeQueries[mrand.IntN(len(probeQueries))]}

	// Our own answer to compare with (cached after the first probe).
	ownSet := map[string]bool{}
	for _, u := range m.probeSearch(ctx, pr.Query) {
		ownSet[u] = true
	}

	m.mu.Lock()
	t := m.tunnels[id]
	var urls []string // direct URLs, then relay URLs (marked with a prefix)
	if p, ok := m.peers[id]; ok {
		if p.URL != "" {
			urls = append(urls, p.URL)
		}
		if r, ok := m.peers[p.Via]; ok && r.URL != "" {
			urls = append(urls, "relay:"+r.URL)
		}
	}
	m.mu.Unlock()
	if hint.URL != "" {
		urls = append(urls, hint.URL)
	}
	for _, r := range hint.Relays {
		urls = append(urls, "relay:"+r)
	}

	check := func(resp probeResp) error {
		pub, err1 := base64.StdEncoding.DecodeString(resp.Pub)
		sig, err2 := base64.StdEncoding.DecodeString(resp.Sig)
		switch {
		case err1 != nil || err2 != nil || len(pub) != ed25519.PublicKeySize:
			return errors.New("malformed probe answer")
		case resp.Nonce != pr.Nonce:
			return errors.New("probe nonce mismatch")
		case IDFor(pub) != id:
			return errors.New("probe answered by a different node")
		case !ed25519.Verify(pub, probeMessage(pr.Nonce, pr.Query, resp.URLs), sig):
			return errors.New("bad probe signature")
		case len(resp.URLs) == 0:
			return errors.New("empty probe answer")
		}
		if len(ownSet) == 0 {
			return nil // our engines are blocked; cannot cross-check
		}
		for _, u := range resp.URLs {
			if ownSet[u] {
				return nil
			}
		}
		return errors.New("probe results share nothing with ours")
	}

	err := errors.New("no route to node")
	if t != nil {
		var resp probeResp
		if resp, err = t.probe(ctx, pr); err == nil {
			if err = check(resp); err == nil {
				return nil
			}
		}
	}
	for _, u := range urls {
		var resp probeResp
		target := u + "/mesh/v1/probe"
		if relay, ok := strings.CutPrefix(u, "relay:"); ok {
			target = relay + "/mesh/v1/relay/" + id + "/probe"
		}
		if err = m.post(ctx, target, pr, &resp); err != nil {
			continue
		}
		if err = check(resp); err == nil {
			return nil
		}
	}
	return err
}
