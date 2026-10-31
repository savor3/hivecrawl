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
	// Never asked, but it reports its own totals: scored from those.
	busy := health(&Peer{Res: &Resources{Searches: 1000, Failed: 1, Cores: 1, Load: 0.5, Engines: 100}})
	overloaded := health(&Peer{Res: &Resources{Searches: 1000, Failed: 400, Cores: 1, Load: 2.5, Engines: 100}})
	if busy < 95 || overloaded >= 60 {
		t.Fatalf("reported: healthy=%d overloaded=%d", busy, overloaded)
	}
	// Once this node has asked it enough, its own record wins.
	if h := health(&Peer{OK: 2, Fail: 8, Res: &Resources{Searches: 1000, Engines: 100}}); h >= 60 {
		t.Fatalf("own record of failures ignored: %d", h)
	}
	// Missing signals are left out rather than counted as zero.
	if h := health(&Peer{OK: 10}); h != 100 {
		t.Fatalf("only answers known, all answered: got %d", h)
	}
}
