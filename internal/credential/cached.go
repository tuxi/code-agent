package credential

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// CachedResolver wraps a Resolver and caches its result. It refreshes the
// credential before it expires.
//
// Refresh strategy:
//   - If the cached credential is not expired and not within the refresh
//     window, it is returned immediately.
//   - If the cached credential is within RefreshWindow of expiry (or already
//     expired), a synchronous refresh is triggered. The call blocks until
//     the inner Resolver returns.
//   - If the inner Resolver returns a Credential with a nil ExpiresAt,
//     CachedResolver falls back to TTL for cache invalidation.
//
// CachedResolver is concurrency-safe: concurrent callers requesting the same
// target share a single refresh invocation.
type CachedResolver struct {
	Inner         Resolver
	TTL           time.Duration // max cache time when ExpiresAt is nil
	RefreshWindow time.Duration // how long before expiry to trigger refresh
	Now           func() time.Time

	mu    sync.Mutex
	cache map[string]*cacheEntry
}

type cacheEntry struct {
	cred       ResolvedCredential
	storedAt   time.Time
	refreshing bool
	cond       *sync.Cond
}

func (r *CachedResolver) Resolve(ctx context.Context, target Target) (ResolvedCredential, error) {
	r.mu.Lock()
	if r.cache == nil {
		r.cache = make(map[string]*cacheEntry)
	}
	r.mu.Unlock()

	key := target.String()
	r.mu.Lock()
	entry, ok := r.cache[key]
	r.mu.Unlock()

	now := r.now()
	if ok && !r.shouldRefresh(entry, now) {
		return entry.cred, nil
	}

	// Serialise refreshes per target: only one caller does the work; others
	// wait for the result.
	r.mu.Lock()
	if e, ok2 := r.cache[key]; ok2 && !r.shouldRefresh(e, now) {
		// Another goroutine refreshed while we waited for the lock.
		r.mu.Unlock()
		return e.cred, nil
	}
	entry, ok = r.cache[key]
	if ok && entry.refreshing {
		r.mu.Unlock()
		return r.waitRefresh(ctx, key)
	}
	if !ok {
		entry = &cacheEntry{refreshing: true}
	} else {
		entry.refreshing = true
	}
	entry.cond = sync.NewCond(&r.mu)
	r.cache[key] = entry
	r.mu.Unlock()

	c, err := r.Inner.Resolve(ctx, target)
	r.mu.Lock()
	entry.refreshing = false
	if err != nil {
		r.mu.Unlock()
		entry.cond.Broadcast()
		return ResolvedCredential{}, err
	}
	entry.cred = c
	entry.cred.Source = fmt.Sprintf("cached(%s)", c.Source)
	entry.storedAt = now
	r.mu.Unlock()
	entry.cond.Broadcast()
	return entry.cred, nil
}

// shouldRefresh reports whether a cached entry needs a refresh at time now.
func (r *CachedResolver) shouldRefresh(e *cacheEntry, now time.Time) bool {
	if e.cred.ExpiresAt != nil {
		expiry := *e.cred.ExpiresAt
		window := r.RefreshWindow
		if window <= 0 {
			window = 30 * time.Second
		}
		return now.After(expiry.Add(-window))
	}
	// No expiry — use TTL.
	ttl := r.TTL
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	return now.After(e.storedAt.Add(ttl))
}

func (r *CachedResolver) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *CachedResolver) waitRefresh(ctx context.Context, key string) (ResolvedCredential, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for {
		entry, ok := r.cache[key]
		if !ok || !entry.refreshing {
			if ok {
				return entry.cred, nil
			}
			return ResolvedCredential{}, nil
		}
		// Wait with context cancellation support.
		done := make(chan struct{})
		go func() {
			entry.cond.Wait()
			close(done)
		}()
		r.mu.Unlock()
		select {
		case <-ctx.Done():
			r.mu.Lock()
			return ResolvedCredential{}, ctx.Err()
		case <-done:
			r.mu.Lock()
		}
	}
}
