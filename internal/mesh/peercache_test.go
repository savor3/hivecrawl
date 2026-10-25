package mesh

import (
	"context"
	"os"
	"testing"
	"time"

	"hivecrawl/internal/netx"
)

// A node remembers public nodes it reached and rejoins through them after a
// restart, even with no seeds configured.
func TestRejoinFromSavedPeers(t *testing.T) {
	a, aURL := publicNode(t, results)
	dir := t.TempDir()
	d := netx.PublicDialer(nil)
	opts := Options{Key: testKey, PeerRate: 100, Dialer: d, Client: netx.Client(d, 5*time.Second), Local: results, NoIPLimits: true, DataDir: dir}

	first := New(withSeeds(opts, aURL))
	first.gossipOnce(context.Background())
	if b, err := os.ReadFile(first.peerCachePath()); err != nil || string(b) != `["`+aURL+`"]` {
		t.Fatalf("saved peers = %s, %v", b, err)
	}

	// Restart with no seeds at all.
	second := New(opts)
	second.loadPeerCache()
	second.gossipOnce(context.Background())
	found := false
	for _, p := range second.Peers() {
		found = found || p.ID == a.ID()
	}
	if !found {
		t.Fatal("did not rejoin through the saved peer")
	}

	// Another network never sees this list.
	other := opts
	other.Key = testKey + "-other"
	if New(other).peerCachePath() == first.peerCachePath() {
		t.Fatal("peer lists must be per network")
	}
}

func withSeeds(o Options, seeds ...string) Options {
	o.Peers = seeds
	return o
}
