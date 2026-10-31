package node

import (
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
