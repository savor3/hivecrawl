package cache

import (
	"testing"
	"time"

	"hivecrawl/internal/types"
)

func TestKeyNormalizesQuery(t *testing.T) {
	a := Key(types.SearchRequest{Query: "  Kerala   Weather ", Lang: "en", Country: "in"})
	b := Key(types.SearchRequest{Query: "kerala weather", Lang: "en", Country: "in", Limit: 5})
	if a != b {
		t.Fatal("equivalent queries got different keys")
	}
}

func TestTTLAndEviction(t *testing.T) {
	c := New(50*time.Millisecond, 2)
	r := []types.Result{{URL: "https://x"}}
	c.Put("a", r, "local")
	c.Put("b", r, "local")
	c.Get("a") // a is now most recent
	c.Put("c", r, "local")
	if _, _, ok := c.Get("b"); ok {
		t.Fatal("least recently used entry not evicted")
	}
	if _, _, ok := c.Get("a"); !ok {
		t.Fatal("recent entry evicted")
	}
	time.Sleep(60 * time.Millisecond)
	if _, _, ok := c.Get("a"); ok {
		t.Fatal("expired entry returned")
	}
}

func TestGetStale(t *testing.T) {
	c := New(20*time.Millisecond, 4)
	c.Put("a", []types.Result{{URL: "https://x"}}, "local")
	time.Sleep(30 * time.Millisecond)
	if _, _, ok := c.Get("a"); ok {
		t.Fatal("expired entry returned by Get")
	}
	if r, _, ok := c.GetStale("a", time.Hour); !ok || len(r) != 1 {
		t.Fatal("stale entry not returned by GetStale")
	}
	if _, _, ok := c.GetStale("a", 10*time.Millisecond); ok {
		t.Fatal("entry older than maxAge returned")
	}
}
