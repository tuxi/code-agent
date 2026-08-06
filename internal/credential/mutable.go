package credential

import (
	"context"
	"sync"
)

// MutableResolver is a concurrency-safe static credential map that can be
// updated at runtime (design-providers-grouped-config.md A2: POST /v1/secrets
// injects provider keys into a running daemon without restarting). Reads are
// cheap (single mutex around a map lookup); writes replace entries wholesale.
//
// It is the "injected" layer of the CredentialResolver chain — the same slot
// embedded hosts fill via secretsJSON at startup, but mutable after start.
type MutableResolver struct {
	mu    sync.RWMutex
	creds map[Target]Credential
}

// NewMutableResolver returns an empty mutable resolver.
func NewMutableResolver() *MutableResolver {
	return &MutableResolver{creds: make(map[Target]Credential)}
}

// Resolve implements Resolver.
func (r *MutableResolver) Resolve(_ context.Context, target Target) (ResolvedCredential, error) {
	r.mu.RLock()
	c, ok := r.creds[target]
	r.mu.RUnlock()
	if !ok {
		return ResolvedCredential{}, nil
	}
	return ResolvedCredential{
		Credential: c,
		Source:     "injected:" + target.String(),
	}, nil
}

// Set upserts one credential.
func (r *MutableResolver) Set(target Target, c Credential) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.creds[target] = c
}

// SetAll replaces the whole map (e.g. a full secrets refresh).
func (r *MutableResolver) SetAll(creds map[Target]Credential) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.creds = creds
}

// Len returns the number of injected credentials (for tests/logging).
func (r *MutableResolver) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.creds)
}
