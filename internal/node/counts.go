package node

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"time"
)

// countsFile keeps the search totals in the data directory, so they add up
// across restarts and updates instead of starting again at zero.
const countsFile = "counts.json"

// countsEvery is how often the totals are written; a crash loses at most
// this much.
const countsEvery = 30 * time.Second

type counts struct {
	Searches int64 `json:"searches"`
	Failed   int64 `json:"failed"`
}

// KeepCounts loads the saved totals from dir and saves them every
// countsEvery until ctx ends. The returned func saves them once more; call
// it on the way out.
func (n *Node) KeepCounts(ctx context.Context, dir string) (save func()) {
	path := filepath.Join(dir, countsFile)
	if b, err := os.ReadFile(path); err == nil {
		var c counts
		if err := json.Unmarshal(b, &c); err == nil {
			n.searches.mu.Lock()
			n.searches.total += c.Searches
			n.searches.mu.Unlock()
			n.failed.mu.Lock()
			n.failed.total += c.Failed
			n.failed.mu.Unlock()
		}
	}
	go func() {
		t := time.NewTicker(countsEvery)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				n.saveCounts(path)
			}
		}
	}()
	return func() { n.saveCounts(path) }
}

func (n *Node) saveCounts(path string) {
	b, _ := json.Marshal(counts{Searches: n.SearchesTotal(), Failed: n.FailedTotal()})
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		log.Printf("saving search counts: %v", err)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		log.Printf("saving search counts: %v", err)
	}
}
