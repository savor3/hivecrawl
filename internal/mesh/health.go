package mesh

import (
	"bufio"
	"os"
	"runtime"
	"strconv"
	"strings"
)

// Resources is what a node reports about its machine in hellos, for the
// network's pool totals. Older nodes send none.
type Resources struct {
	Cores     int     `json:"cores,omitempty"`
	MemMB     int     `json:"memMb,omitempty"`
	MemFreeMB int     `json:"memFreeMb,omitempty"`
	Load      float64 `json:"load,omitempty"` // 1-minute load average
	Engines   int     `json:"engines,omitempty"`
	// Searches this node answered (API, MCP and for peers): in the last
	// minute, and in all (kept across restarts); and how many of those
	// got nothing back.
	SearchesPerMin int   `json:"searchesPerMin,omitempty"`
	Searches       int64 `json:"searches,omitempty"`
	Failed         int64 `json:"failed,omitempty"`
}

// HostResources reads this machine's cores, memory and load. Inside a
// container /proc shows the host, which is what the pool totals want.
func HostResources() Resources {
	r := Resources{Cores: runtime.NumCPU()}
	if f, err := os.Open("/proc/meminfo"); err == nil {
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			k, v, _ := strings.Cut(sc.Text(), ":")
			kb, _ := strconv.Atoi(strings.TrimSuffix(strings.TrimSpace(v), " kB"))
			switch k {
			case "MemTotal":
				r.MemMB = kb / 1024
			case "MemAvailable":
				r.MemFreeMB = kb / 1024
			}
		}
		f.Close()
	}
	if b, err := os.ReadFile("/proc/loadavg"); err == nil {
		if f := strings.Fields(string(b)); len(f) > 0 {
			r.Load, _ = strconv.ParseFloat(f[0], 64)
		}
	}
	return r
}

// Health weights: how a peer scores, from what this node saw of it.
const (
	healthAnswers   = 0.4 // share of searches it answered
	healthLatency   = 0.2 // full marks up to goodLatencyMs, none at badLatencyMs
	healthAgreement = 0.2 // its results match other routes (not junk)
	healthUnblocked = 0.2 // share of its engines not blocked

	goodLatencyMs = 200
	badLatencyMs  = 3000
	// defaultEngines is assumed for nodes too old to report their count.
	defaultEngines = 10
	// minAsked is how many searches this node must have sent a peer before
	// its own record of them outweighs the peer's reported totals.
	minAsked = 5
)

// health scores a peer 0..100, or -1 while there is nothing to go on.
//
// The best evidence is this node's own record of searches it sent the
// peer. Nodes that answer from their own engines rarely ask each other,
// so without that record the score uses what the peer reports about
// itself in gossip: the share of its searches that did not fail, and how
// busy its machine is in place of latency. Signals with no data yet are
// left out and the remaining weights rescaled, so a peer is not marked
// down for them.
func health(p *Peer) int {
	n := p.OK + p.Fail
	reported := p.Res != nil && p.Res.Searches > 0
	if n == 0 && !reported {
		return -1
	}
	var sum, w float64
	add := func(weight, v float64) {
		sum += weight * min(max(v, 0), 1)
		w += weight
	}
	if n >= minAsked || !reported {
		add(healthAnswers, float64(p.OK)/float64(n))
	} else {
		add(healthAnswers, 1-float64(p.Res.Failed)/float64(p.Res.Searches))
	}
	switch {
	case p.LatencyMs > 0:
		add(healthLatency, 1-float64(p.LatencyMs-goodLatencyMs)/float64(badLatencyMs-goodLatencyMs))
	case p.Res != nil && p.Res.Cores > 0:
		// Full marks up to fully busy, none at twice its cores.
		add(healthLatency, 2-p.Res.Load/float64(p.Res.Cores))
	}
	if p.Checks > 0 {
		add(healthAgreement, p.Agreement)
	}
	engines := defaultEngines
	if p.Res != nil && p.Res.Engines > 0 {
		engines = p.Res.Engines
	}
	add(healthUnblocked, 1-float64(len(p.Blocked))/float64(max(engines, len(p.Blocked))))
	return int(sum/w*100 + 0.5)
}
