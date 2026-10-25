package mesh

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"hivecrawl/internal/types"
)

func TestLimiterPerNodeAndRate(t *testing.T) {
	l := newLimiter()
	for i := range 3 {
		if !l.allow("a", 3) {
			t.Fatalf("request %d denied", i)
		}
	}
	if l.allow("a", 3) {
		t.Fatal("4th request allowed at rate 3")
	}
	if !l.allow("b", 3) {
		t.Fatal("other node limited by a's usage")
	}
}

func TestMultiplier(t *testing.T) {
	cases := []struct {
		name             string
		answered, served int
		online           time.Duration
		want             float64
	}{
		{"new node gets the base rate", 0, 0, 0, 1},
		{"10 answers to us earns +1x", 10, 0, 0, 2},
		{"bonus is capped at 3x", 500, 0, 0, 3},
		{"long uptime adds a bonus", 10, 0, 2 * time.Hour, 2.25},
		{"pure freeloader is halved", 0, 200, 0, 0.5},
		{"some asking without answering is fine", 0, 50, 0, 1},
	}
	for _, c := range cases {
		if got := multiplier(c.answered, c.served, 30, c.online); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}

// When the node is busy, a peer that has answered our searches waits for a
// slot and is served; a peer that never helped is turned away immediately.
func TestBusyNodeServesContributorsFirst(t *testing.T) {
	release := make(chan struct{})
	var mu sync.Mutex
	servedFor := map[string]int{}
	m := New(Options{Key: testKey, PeerRate: 1000, MaxConcurrent: 1,
		Local: func(ctx context.Context, req types.SearchRequest) (types.SearchResponse, error) {
			if req.Query == "slow" {
				<-release
			}
			return types.SearchResponse{Success: true}, nil
		}})
	m.mu.Lock()
	m.peer("helper").OK = 20 // answered 20 of our searches -> priority 3x
	m.peer("stranger")
	markVerified(m, "filler", "helper", "stranger")
	m.mu.Unlock()

	// Fill the only slot.
	go m.serveSearch(context.Background(), "filler", route{}, types.SearchRequest{Query: "slow"})
	time.Sleep(50 * time.Millisecond)

	if _, err := m.serveSearch(context.Background(), "stranger", route{}, types.SearchRequest{Query: "q"}); !errors.Is(err, errBusy) {
		t.Fatalf("stranger: want errBusy while full, got %v", err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := m.serveSearch(context.Background(), "helper", route{}, types.SearchRequest{Query: "q"})
		mu.Lock()
		servedFor["helper"]++
		mu.Unlock()
		done <- err
	}()
	time.Sleep(100 * time.Millisecond)
	close(release) // slot frees up; the waiting helper gets it
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("helper: want served after waiting, got %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("helper never got a slot")
	}
	for _, p := range m.Peers() {
		if p.ID == "helper" && p.Priority != 3 {
			t.Fatalf("helper priority = %v, want 3", p.Priority)
		}
	}
}

func TestContributorGetsHigherRateLimit(t *testing.T) {
	m := New(Options{Key: testKey, PeerRate: 5,
		Local: func(context.Context, types.SearchRequest) (types.SearchResponse, error) {
			return types.SearchResponse{Success: true}, nil
		}})
	m.mu.Lock()
	m.peer("helper").OK = 20
	markVerified(m, "helper", "stranger")
	m.mu.Unlock()
	count := func(id string) int {
		n := 0
		for range 50 {
			if _, err := m.serveSearch(context.Background(), id, route{}, types.SearchRequest{Query: "q"}); err == nil {
				n++
			}
		}
		return n
	}
	if s, h := count("stranger"), count("helper"); s != 5 || h != 15 {
		t.Fatalf("served stranger=%d helper=%d, want 5 and 15 (base rate 5, helper 3x)", s, h)
	}
}

// markVerified skips the probe for rate-limit tests. Caller holds mu.
func markVerified(m *Mesh, ids ...string) {
	for _, id := range ids {
		m.verified[id] = time.Now().Add(time.Hour)
	}
}
