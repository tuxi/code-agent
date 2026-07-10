package credential

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestCachedResolverReturnsFromCache(t *testing.T) {
	var calls int32
	inner := &countingResolver{
		fn: func(_ context.Context, _ Target) (ResolvedCredential, error) {
			atomic.AddInt32(&calls, 1)
			return ResolvedCredential{
				Credential: Credential{Type: Bearer, Secret: "tok"},
				Source:     "inner",
			}, nil
		},
	}

	now := time.Now()
	r := &CachedResolver{
		Inner: inner,
		TTL:   10 * time.Minute,
		Now:   func() time.Time { return now },
	}

	target := Target{Namespace: "gateway", Name: "default"}
	ctx := context.Background()

	// First call — should call inner.
	c1, err := r.Resolve(ctx, target)
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	if c1.Secret != "tok" {
		t.Errorf("Secret = %q", c1.Secret)
	}
	if atomic.LoadInt32(&calls) != 1 {
		t.Errorf("calls = %d, want 1", calls)
	}

	// Second call with same target within TTL — should use cache.
	c2, err := r.Resolve(ctx, target)
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if c2.Secret != "tok" {
		t.Errorf("Secret = %q", c2.Secret)
	}
	if atomic.LoadInt32(&calls) != 1 {
		t.Errorf("calls = %d, want 1 (cached)", calls)
	}
}

func TestCachedResolverRefreshOnExpiry(t *testing.T) {
	var calls int32
	var secret atomic.Value
	secret.Store("tok-v1")

	baseTime := time.Now()
	var clock atomic.Value
	clock.Store(baseTime)

	inner := &countingResolver{
		fn: func(_ context.Context, _ Target) (ResolvedCredential, error) {
			atomic.AddInt32(&calls, 1)
			// Use the fake clock for ExpiresAt so it aligns with shouldRefresh's now.
			expiry := clock.Load().(time.Time).Add(1 * time.Hour)
			return ResolvedCredential{
				Credential: Credential{
					Type:      Bearer,
					Secret:    secret.Load().(string),
					ExpiresAt: &expiry,
				},
				Source: "inner",
			}, nil
		},
	}

	r := &CachedResolver{
		Inner:         inner,
		RefreshWindow: 5 * time.Minute,
		Now:           func() time.Time { return clock.Load().(time.Time) },
	}

	target := Target{Namespace: "gateway", Name: "default"}
	ctx := context.Background()

	// First call at baseTime — expiry is baseTime + 1h.
	c1, _ := r.Resolve(ctx, target)
	if c1.Secret != "tok-v1" {
		t.Fatalf("first Secret = %q", c1.Secret)
	}

	// Advance clock past the refresh window: baseTime + 56 min.
	// Expiry is baseTime + 60 min, window is 5 min → refresh after baseTime + 55 min.
	clock.Store(baseTime.Add(56 * time.Minute))
	secret.Store("tok-v2")

	// Second call — should trigger refresh.
	c2, err := r.Resolve(ctx, target)
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if c2.Secret != "tok-v2" {
		t.Errorf("Secret = %q, want %q", c2.Secret, "tok-v2")
	}
	if n := atomic.LoadInt32(&calls); n != 2 {
		t.Errorf("calls = %d, want 2 (refreshed)", n)
	}
}

func TestCachedResolverSourceAnnotation(t *testing.T) {
	inner := StaticResolver{
		{Namespace: "llm", Name: "deepseek"}: {Type: Bearer, Secret: "sk"},
	}
	r := &CachedResolver{
		Inner: inner,
		TTL:   1 * time.Hour,
		Now:   time.Now,
	}
	ctx := context.Background()
	c, err := r.Resolve(ctx, Target{Namespace: "llm", Name: "deepseek"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "cached(static:llm/deepseek)"
	if c.Source != want {
		t.Errorf("Source = %q, want %q", c.Source, want)
	}
}

// countingResolver wraps a resolve function and counts calls.
type countingResolver struct {
	fn func(context.Context, Target) (ResolvedCredential, error)
}

func (r *countingResolver) Resolve(ctx context.Context, target Target) (ResolvedCredential, error) {
	return r.fn(ctx, target)
}
