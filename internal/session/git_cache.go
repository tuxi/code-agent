package session

import (
	"sync"
	"time"
)

// FastGitProvider caches git status output with a TTL and a dirty flag so
// repeated calls within a turn avoid redundant git(1) invocations. The agent
// loop calls GitInfo on every model request; in a 10-tool-call turn that is
// 10+ execs, each with a 5s timeout, even though the git state cannot change
// between consecutive reads. A write tool (bash with a mutating git command,
// file write) marks the cache dirty so the next read refreshes.
//
// Zero-value is unusable; construct with NewFastGitProvider.
type FastGitProvider struct {
	mu sync.RWMutex

	workspaceRoot string
	cachedInfo    string
	lastUpdate    time.Time
	ttl           time.Duration
	dirty         bool
}

// NewFastGitProvider creates a git-info cache for the given workspace. ttl is
// how long a cached result is considered fresh when the dirty flag is clear
// (suggested: 30s — longer than a turn, shorter than a human pause).
func NewFastGitProvider(workspaceRoot string, ttl time.Duration) *FastGitProvider {
	return &FastGitProvider{
		workspaceRoot: workspaceRoot,
		ttl:           ttl,
		dirty:         true, // force first read
	}
}

// MarkDirty forces the next GitInfo call to refresh. Call this after any tool
// execution that may have changed the git repository (bash with git commands,
// file writes, etc.).
func (p *FastGitProvider) MarkDirty() {
	p.mu.Lock()
	p.dirty = true
	p.mu.Unlock()
}

// GitInfo returns a cached snapshot of the git repository state. It refreshes
// only when the cache is dirty or the TTL has expired. Returns "" when the
// workspace is not a git repository or git is not installed.
func (p *FastGitProvider) GitInfo() string {
	p.mu.RLock()
	if !p.dirty && time.Since(p.lastUpdate) < p.ttl {
		defer p.mu.RUnlock()
		return p.cachedInfo
	}
	p.mu.RUnlock()

	p.mu.Lock()
	defer p.mu.Unlock()

	// Double-check under write lock.
	if !p.dirty && time.Since(p.lastUpdate) < p.ttl {
		return p.cachedInfo
	}

	p.cachedInfo = gitInfoSnapshot(p.workspaceRoot)
	p.lastUpdate = time.Now()
	p.dirty = false
	return p.cachedInfo
}

// gitInfoSnapshot runs the actual git commands. Delegates to the existing
// GitInfo helper in builder.go so the logic lives in one place.
func gitInfoSnapshot(workspaceRoot string) string {
	return GitInfo(workspaceRoot)
}
