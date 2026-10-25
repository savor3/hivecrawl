package node

import (
	"testing"

	"hivecrawl/internal/mesh"
	"hivecrawl/internal/types"
)

func TestAssignSplitsAndAvoidsBlocked(t *testing.T) {
	cands := []mesh.Peer{
		{ID: "home", Blocked: []string{"duckduckgo"}},
		{ID: "srv1"},
		{ID: "srv2"},
		{ID: "srv3"}, // beyond maxPeers
	}
	got := map[string][]string{}
	for _, a := range assign([]string{"google cse", "brave", "duckduckgo", "bing"}, cands, 3) {
		got[a.peer.ID] = a.engines
	}
	if _, ok := got["srv3"]; ok {
		t.Fatal("assigned to a peer beyond maxPeers")
	}
	for _, e := range got["home"] {
		if e == "duckduckgo" {
			t.Fatal("duckduckgo sent to the peer that reported it blocked")
		}
	}
	total := 0
	for id, es := range got {
		total += len(es)
		if len(es) > 2 {
			t.Fatalf("%s got %v: load not spread", id, es)
		}
	}
	if total != 4 {
		t.Fatalf("every engine must be assigned exactly once, got %v", got)
	}
}

func TestAssignBlockedEverywhereStillTries(t *testing.T) {
	cands := []mesh.Peer{{ID: "a", Blocked: []string{"brave"}}, {ID: "b", Blocked: []string{"brave"}}}
	as := assign([]string{"brave"}, cands, 3)
	if len(as) != 1 || as[0].engines[0] != "brave" {
		t.Fatalf("brave should still be tried once (reports can be stale), got %+v", as)
	}
}

func TestAssignNoPeers(t *testing.T) {
	if as := assign([]string{"brave"}, nil, 3); as != nil {
		t.Fatalf("want nil, got %+v", as)
	}
}

func TestReportAgreementFlagsJunkPeer(t *testing.T) {
	m := mesh.New(mesh.Options{Key: "0123456789abcdef-x"})
	n := &Node{Mesh: m}
	honest, junk := mesh.NewIdentity().ID, mesh.NewIdentity().ID
	m.TestAddPeer(honest)
	m.TestAddPeer(junk)
	local := []types.Result{{URL: "https://go.dev/a"}, {URL: "https://go.dev/b"}}
	for range 6 {
		n.reportAgreement(local, []part{
			{peer: honest, results: []types.Result{{URL: "https://go.dev/a"}, {URL: "https://x.dev"}}},
			{peer: junk, results: []types.Result{{URL: "https://spam1.biz"}, {URL: "https://spam2.biz"}}},
		})
	}
	for _, p := range m.Peers() {
		switch p.ID {
		case honest:
			if p.Agreement < 0.4 {
				t.Fatalf("honest peer agreement %v", p.Agreement)
			}
		case junk:
			if p.Agreement != 0 || p.Checks != 6 {
				t.Fatalf("junk peer agreement %v checks %d", p.Agreement, p.Checks)
			}
		}
	}
	// The junk peer is now tried after the honest one.
	c := m.Candidates()
	if len(c) != 2 || c[0].ID != honest {
		t.Fatalf("candidates = %+v, want honest first", c)
	}
}
