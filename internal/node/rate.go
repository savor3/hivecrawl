package node

import (
	"sync"
	"time"
)

// rate counts events over the last minute in one-second buckets.
type rate struct {
	mu      sync.Mutex
	count   [60]int
	second  [60]int64
	total   int64
	nowFunc func() time.Time // for tests
}

func (r *rate) now() time.Time {
	if r.nowFunc != nil {
		return r.nowFunc()
	}
	return time.Now()
}

func (r *rate) add() {
	s := r.now().Unix()
	r.mu.Lock()
	defer r.mu.Unlock()
	if i := s % 60; r.second[i] != s {
		r.second[i], r.count[i] = s, 1
	} else {
		r.count[i]++
	}
	r.total++
}

// perMinute is how many events happened in the last 60 seconds.
func (r *rate) perMinute() int {
	s := r.now().Unix()
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for i := range r.count {
		if s-r.second[i] < 60 {
			n += r.count[i]
		}
	}
	return n
}

// CountSearch records one search this node answered: an API or MCP
// search, or one a peer asked for. Internal sub-searches are not counted.
func (n *Node) CountSearch() { n.searches.add() }

// SearchesPerMin is how many searches this node answered in the last minute.
func (n *Node) SearchesPerMin() int { return n.searches.perMinute() }

// SearchesTotal is how many searches this node answered since it started.
func (n *Node) SearchesTotal() int64 {
	n.searches.mu.Lock()
	defer n.searches.mu.Unlock()
	return n.searches.total
}
