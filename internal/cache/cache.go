// Package cache is a small TTL + LRU cache for search results.
package cache

import (
	"container/list"
	"sort"
	"strings"
	"sync"
	"time"

	"hivecrawl/internal/types"
)

type entry struct {
	key     string
	results []types.Result
	source  string
	expires time.Time
}

type Cache struct {
	mu    sync.Mutex
	ttl   time.Duration
	max   int
	ll    *list.List
	items map[string]*list.Element

	hits, misses uint64
}

func New(ttl time.Duration, max int) *Cache {
	return &Cache{ttl: ttl, max: max, ll: list.New(), items: map[string]*list.Element{}}
}

// Key normalizes a request into a cache key. Limit is not part of the key:
// the full result list is cached and trimmed on read.
func Key(req types.SearchRequest) string {
	q := strings.ToLower(strings.Join(strings.Fields(req.Query), " "))
	key := q + "\x00" + req.Lang + "\x00" + req.Country + "\x00" + req.TimeRange + "\x00" + req.Category
	if len(req.Engines) > 0 {
		e := append([]string(nil), req.Engines...)
		sort.Strings(e)
		key += "\x00" + strings.Join(e, ",")
	}
	return key
}

func (c *Cache) Get(key string) ([]types.Result, string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[key]
	if !ok {
		c.misses++
		return nil, "", false
	}
	e := el.Value.(*entry)
	if time.Now().After(e.expires) {
		// Kept (until LRU eviction) so GetStale can still serve it when
		// every live route fails.
		c.misses++
		return nil, "", false
	}
	c.ll.MoveToFront(el)
	c.hits++
	return e.results, e.source, true
}

// GetStale returns an entry even after its TTL, as long as it was stored
// within maxAge. It is the last resort when no route can answer a query:
// an older answer beats an empty one.
func (c *Cache) GetStale(key string, maxAge time.Duration) ([]types.Result, string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[key]
	if !ok {
		return nil, "", false
	}
	e := el.Value.(*entry)
	if time.Since(e.expires.Add(-c.ttl)) > maxAge {
		return nil, "", false
	}
	return e.results, e.source, true
}

func (c *Cache) Put(key string, results []types.Result, source string) {
	if c.max <= 0 || len(results) == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[key]; ok {
		e := el.Value.(*entry)
		e.results, e.source, e.expires = results, source, time.Now().Add(c.ttl)
		c.ll.MoveToFront(el)
		return
	}
	c.items[key] = c.ll.PushFront(&entry{key: key, results: results, source: source, expires: time.Now().Add(c.ttl)})
	for c.ll.Len() > c.max {
		old := c.ll.Back()
		c.ll.Remove(old)
		delete(c.items, old.Value.(*entry).key)
	}
}

type Stats struct {
	Entries int    `json:"entries"`
	Hits    uint64 `json:"hits"`
	Misses  uint64 `json:"misses"`
}

func (c *Cache) Stats() Stats {
	c.mu.Lock()
	defer c.mu.Unlock()
	return Stats{Entries: c.ll.Len(), Hits: c.hits, Misses: c.misses}
}
