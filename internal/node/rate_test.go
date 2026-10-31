package node

import (
	"context"
	"testing"
	"time"
)

func TestRate(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	r := &rate{nowFunc: func() time.Time { return now }}
	for range 5 {
		r.add()
	}
	now = now.Add(30 * time.Second)
	r.add()
	if got := r.perMinute(); got != 6 {
		t.Fatalf("after 30s: %d, want 6", got)
	}
	now = now.Add(45 * time.Second) // the first five are now 75s old
	if got := r.perMinute(); got != 1 {
		t.Fatalf("after 75s: %d, want 1", got)
	}
	now = now.Add(60 * time.Second)
	if got := r.perMinute(); got != 0 || r.total != 6 {
		t.Fatalf("later: %d per minute, %d total", got, r.total)
	}
}

// Totals saved in the data directory add up across restarts.
func TestCountsSurviveRestart(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	a := New("a", nil, nil, 1)
	save := a.KeepCounts(ctx, dir)
	for range 3 {
		a.CountSearch()
	}
	a.CountFailed()
	cancel()
	save()

	b := New("b", nil, nil, 1)
	b.KeepCounts(context.Background(), dir)
	b.CountSearch()
	if b.SearchesTotal() != 4 || b.FailedTotal() != 1 {
		t.Fatalf("after restart: %d searches, %d failed; want 4 and 1", b.SearchesTotal(), b.FailedTotal())
	}
}
