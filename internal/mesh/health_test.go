package mesh

import "testing"

func TestHealth(t *testing.T) {
	if h := health(&Peer{}); h != -1 {
		t.Fatalf("never asked: got %d, want -1", h)
	}
	good := health(&Peer{OK: 99, Fail: 1, LatencyMs: 150, Checks: 5, Agreement: 0.9, Res: &Resources{Engines: 100}})
	slow := health(&Peer{OK: 99, Fail: 1, LatencyMs: 2500, Checks: 5, Agreement: 0.9, Res: &Resources{Engines: 100}})
	failing := health(&Peer{OK: 20, Fail: 80, LatencyMs: 150, Blocked: []string{"brave", "duckduckgo", "google cse"}})
	if good < 95 || slow >= good || failing >= 60 {
		t.Fatalf("good=%d slow=%d failing=%d", good, slow, failing)
	}
	// Missing signals are left out rather than counted as zero.
	if h := health(&Peer{OK: 10}); h != 100 {
		t.Fatalf("only answers known, all answered: got %d", h)
	}
}
