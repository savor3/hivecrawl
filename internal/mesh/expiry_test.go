package mesh

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func shown(m *Mesh, id string) bool {
	for _, p := range m.Peers() {
		if p.ID == id {
			return true
		}
	}
	return false
}

// A node that has gone away but is still mentioned by other nodes must not
// show, be searched, or be passed on, and must leave the table.
func TestDeadNodeIsNotKeptAliveByGossip(t *testing.T) {
	a, _ := publicNode(t, results)
	b, bURL := publicNode(t, results)
	gone := httptest.NewServer(http.NotFoundHandler())
	deadURL := gone.URL
	gone.Close()
	dead := NewIdentity().ID

	// b vouches for the dead node (as an older node or a stale table would).
	a.learn(bURL, helloResp{Self: helloMsg{ID: b.ID(), Name: "b", URL: bURL}, Peers: []Peer{{ID: dead, Name: "dead", URL: deadURL}}})
	if !shown(a, b.ID()) {
		t.Fatal("the node we just reached is not shown")
	}
	if shown(a, dead) {
		t.Fatal("a node only heard of is shown before we reached it")
	}
	for _, p := range a.Candidates() {
		if p.ID == dead {
			t.Fatal("an unreached node is used for searches")
		}
	}

	// Gossip tries it and fails; it stays unreached and is not passed on.
	a.gossipOnce(context.Background())
	var hr helloResp
	c, _ := publicNode(t, results)
	if err := c.post(context.Background(), a.publicURL+"/mesh/v1/hello", c.selfHello(), &hr); err != nil {
		t.Fatal(err)
	}
	for _, p := range hr.Peers {
		if p.ID == dead {
			t.Fatal("hello passed on a node we never reached")
		}
	}

	// Its one window runs out and it is dropped; b is kept, we reached it.
	a.mu.Lock()
	a.peers[dead].LastSeen = time.Now().Add(-peerExpiry - time.Second)
	a.mu.Unlock()
	a.expire()
	a.mu.Lock()
	_, still := a.peers[dead]
	a.mu.Unlock()
	if still {
		t.Fatal("dead node not expired")
	}
	if !shown(a, b.ID()) {
		t.Fatal("live node dropped")
	}
}

// A node we reached and that then stops answering disappears from the list,
// and requesters that asked long ago are not listed forever.
func TestStoppedNodeAndOldRequestersLeaveTheList(t *testing.T) {
	a, _ := publicNode(t, results)
	b, bURL := publicNode(t, results)
	a.seeds = []string{bURL}
	a.gossipOnce(context.Background())
	if !shown(a, b.ID()) {
		t.Fatal("reachable node not shown")
	}
	a.mu.Lock()
	a.peers[b.ID()].LastSeen = time.Now().Add(-peerExpiry - time.Second)
	a.served["hcold"], a.servedAt["hcold"] = 3, time.Now().Add(-time.Hour)
	a.served["hcnew"], a.servedAt["hcnew"] = 1, time.Now()
	a.mu.Unlock()
	a.expire()
	if shown(a, b.ID()) {
		t.Fatal("node that stopped answering is still shown")
	}
	if shown(a, "hcold") || !shown(a, "hcnew") {
		t.Fatal("relay-only requesters: want only the recent one listed")
	}
}
