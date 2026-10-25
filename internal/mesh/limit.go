package mesh

import (
	"context"
	"errors"
	"sync"
	"time"
)

// limiter is a per-node token bucket. Each call passes that node's current
// rate (searches per minute), so a peer's allowance can grow or shrink with
// its contribution without resetting its bucket.
type limiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
}

type bucket struct {
	tokens float64
	last   time.Time
}

func newLimiter() *limiter {
	return &limiter{buckets: map[string]*bucket{}}
}

func (l *limiter) allow(id string, perMinute float64) bool {
	if perMinute <= 0 {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	b, ok := l.buckets[id]
	if !ok {
		b = &bucket{tokens: perMinute, last: now}
		l.buckets[id] = b
	}
	b.tokens = min(b.tokens+now.Sub(b.last).Minutes()*perMinute, perMinute)
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// Priority is reciprocity, as in BitTorrent's tit-for-tat: a peer that has
// answered our searches, and stays online, gets a bigger allowance and goes
// first when this node is busy. Each node judges only from what it has
// seen itself (answers it actually received), so the score can't be faked
// by a peer claiming to contribute.
const (
	answersPerStep   = 10              // each 10 answers to us adds +1x
	maxMultiplier    = 3.0             // cap on contribution bonus
	uptimeBonusAfter = time.Hour       // continuous uptime that earns a bonus
	uptimeBonus      = 0.25            // added to the multiplier
	freeloaderAfter  = 3               // served > answered + 3x base rate → halved
	highPriority     = 1.5             // multiplier at or above this waits for a slot
	slotWait         = 5 * time.Second // how long a high-priority peer may wait
)

// multiplier scales a peer's rate limit by its contribution to us.
// answered: searches it answered for us; served: searches we answered for it.
func multiplier(answered, served, baseRate int, online time.Duration) float64 {
	m := 1.0 + float64(answered)/answersPerStep
	if m > maxMultiplier {
		m = maxMultiplier
	}
	if answered == 0 && served > freeloaderAfter*baseRate {
		m = 0.5
	}
	if online >= uptimeBonusAfter {
		m += uptimeBonus
	}
	return m
}

var errBusy = errors.New("node busy: try another peer")

// slots bounds concurrent searches served for peers. When it is full,
// high-priority peers wait briefly for a slot and others are turned away.
type slots chan struct{}

func (s slots) acquire(ctx context.Context, high bool) error {
	select {
	case s <- struct{}{}:
		return nil
	default:
	}
	if !high {
		return errBusy
	}
	t := time.NewTimer(slotWait)
	defer t.Stop()
	select {
	case s <- struct{}{}:
		return nil
	case <-t.C:
		return errBusy
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s slots) release() { <-s }
