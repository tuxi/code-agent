// Package cache provides an in-memory URL content cache with TTL-based expiry.
// It is used by web_fetch to avoid redundant HTTP round-trips for the same URL
// within a configurable window.
package cache

import (
	"sync"
	"time"
)

// Entry holds a cached response for a single URL.
type Entry struct {
	Body        []byte
	ContentType string
	CachedAt    time.Time
}

// Cache is a concurrency-safe, in-memory URL → content cache. A zero TTL
// disables caching entirely (Load always returns nil, Store is a no-op).
type Cache struct {
	mu    sync.RWMutex
	items map[string]Entry
	ttl   time.Duration
}

// New returns a Cache with the given TTL. If ttl <= 0, the cache is effectively
// disabled — Store is a no-op and Load always returns nil — so callers don't
// need to branch on a nil cache pointer.
func New(ttl time.Duration) *Cache {
	return &Cache{
		items: make(map[string]Entry),
		ttl:   ttl,
	}
}

// Load returns the cached entry for url if it exists and is not expired. Returns
// nil, false when the entry is missing or stale. Expired entries are evicted
// eagerly on access to bound memory.
func (c *Cache) Load(url string) (*Entry, bool) {
	if c.ttl <= 0 {
		return nil, false
	}
	c.mu.RLock()
	e, ok := c.items[url]
	c.mu.RUnlock()
	if !ok {
		return nil, false
	}
	if time.Since(e.CachedAt) > c.ttl {
		c.mu.Lock()
		delete(c.items, url)
		c.mu.Unlock()
		return nil, false
	}
	return &e, true
}

// Store saves an entry in the cache under url. No-op when the cache TTL is zero.
func (c *Cache) Store(url string, body []byte, contentType string) {
	if c.ttl <= 0 {
		return
	}
	c.mu.Lock()
	c.items[url] = Entry{
		Body:        body,
		ContentType: contentType,
		CachedAt:    time.Now(),
	}
	c.mu.Unlock()
}
