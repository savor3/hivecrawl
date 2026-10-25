package mesh

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"hivecrawl/internal/netx"
	"hivecrawl/internal/types"
)

// results returns the same deterministic results any honest node would.
func results(_ context.Context, req types.SearchRequest) (types.SearchResponse, error) {
	var web []types.Result
	for i := range 5 {
		web = append(web, types.Result{URL: fmt.Sprintf("https://example.com/%s/%d", req.Query, i)})
	}
	return types.SearchResponse{Success: true, Data: types.SearchData{Web: web}}, nil
}

func publicNode(t *testing.T, local LocalSearch) (*Mesh, string) {
	var m *Mesh
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mux := http.NewServeMux()
		m.Register(mux)
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	d := netx.PublicDialer(nil)
	m = New(Options{Key: testKey, PeerRate: 100, PublicURL: srv.URL, Dialer: d, Client: netx.Client(d, 5*time.Second), Local: local, NoIPLimits: true})
	return m, srv.URL
}

func waitVerified(m *Mesh, id string) bool {
	for range 100 {
		m.mu.Lock()
		ok := m.verifiedLocked(id)
		m.mu.Unlock()
		if ok {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

// A real node that asks for searches gets probed back, answers, and is verified.
func TestRealNodeGetsVerified(t *testing.T) {
	server, _ := publicNode(t, results)
	asker, askerURL := publicNode(t, results)

	_, err := server.serveSearch(context.Background(), asker.ID(), route{URL: askerURL}, types.SearchRequest{Query: "q"})
	if err != nil {
		t.Fatalf("first search (trial) refused: %v", err)
	}
	if !waitVerified(server, asker.ID()) {
		t.Fatal("real node was not verified by probe")
	}
	for i := range 20 { // well past the trial allowance
		if _, err := server.serveSearch(context.Background(), asker.ID(), route{URL: askerURL}, types.SearchRequest{Query: "q"}); err != nil {
			t.Fatalf("verified node refused at search %d: %v", i, err)
		}
	}
}

// A script holding a valid identity but not running a node gets only the trial.
func TestTakeOnlyClientStaysOnTrial(t *testing.T) {
	server, _ := publicNode(t, results)
	freeloader := NewIdentity()
	served := 0
	var lastErr error
	for range 20 {
		if _, err := server.serveSearch(context.Background(), freeloader.ID, route{URL: "http://127.0.0.1:1"}, types.SearchRequest{Query: "q"}); err == nil {
			served++
		} else {
			lastErr = err
		}
	}
	if served != trialPerMinute || !errors.Is(lastErr, errUnverified) {
		t.Fatalf("freeloader served %d (want %d trial), last error %v", served, trialPerMinute, lastErr)
	}
}

// Pointing the probe at someone else's real node does not verify you: the
// answer is signed with that node's key, not yours.
func TestCannotBorrowAnotherNodesProbe(t *testing.T) {
	server, _ := publicNode(t, results)
	_, honestURL := publicNode(t, results)
	attacker := NewIdentity()
	if err := server.probe(context.Background(), attacker.ID, route{URL: honestURL}); err == nil {
		t.Fatal("attacker verified by redirecting the probe to an honest node")
	}
}

// A node that answers with made-up links (nothing in common with real
// results) fails the probe.
func TestFakeResultsFailProbe(t *testing.T) {
	server, _ := publicNode(t, results)
	liar, liarURL := publicNode(t, func(context.Context, types.SearchRequest) (types.SearchResponse, error) {
		return types.SearchResponse{Success: true, Data: types.SearchData{Web: []types.Result{{URL: "https://spam.biz/1"}}}}, nil
	})
	if err := server.probe(context.Background(), liar.ID(), route{URL: liarURL}); err == nil {
		t.Fatal("node returning fake results passed the probe")
	}
}
