package session

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"sync"
	"time"

	"code-agent/internal/model"
	"code-agent/internal/worktree"
)

// MemoryStore is a pure in-memory implementation of Store. It is the
// contract-canonical test backend: every Store-consuming test can use it
// instead of SQLite. It is NOT safe for concurrent use from multiple
// goroutines — the mutex serialises callers, matching the single-connection
// semantics of SQLiteStore.
//
// Compile-time checks.
var (
	_ Store               = (*MemoryStore)(nil)
	_ SessionStore        = (*MemoryStore)(nil)
	_ EventStore          = (*MemoryStore)(nil)
	_ EventAttentionStore = (*MemoryStore)(nil)
	_ TelemetryStore      = (*MemoryStore)(nil)
)

type MemoryStore struct {
	mu        sync.Mutex
	sessions  map[string]*Session        // id → session (owned copy)
	metas     []Meta                     // ordered by UpdatedAt desc
	events    []EventRecord              // all events, insertion order
	eventSeq  int64                      // store-wide monotonic event seq (mirrors sqlite rowid)
	requests  []RequestRecord            // all requests, insertion order
	worktrees map[string]worktree.Record // client request id → managed worktree reservation
	closed    bool
}

// NewMemoryStore returns an empty MemoryStore ready for use.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		sessions:  make(map[string]*Session),
		worktrees: make(map[string]worktree.Record),
	}
}

// ── SessionStore ──────────────────────────────────────────────────────────

func (m *MemoryStore) Save(ctx context.Context, s *Session) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return errors.New("memory store is closed")
	}
	if s.ID == "" {
		return errors.New("save: session has no ID")
	}
	// Deep-copy so the caller can mutate the original without corrupting the store.
	sess := deepCopySession(s)
	m.sessions[sess.ID] = sess
	m.reindexMetas()
	return nil
}

func (m *MemoryStore) Load(_ context.Context, id string) (*Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	if !ok {
		return nil, fmt.Errorf("session %q not found", id)
	}
	return deepCopySession(s), nil
}

func (m *MemoryStore) List(_ context.Context) ([]Meta, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Meta, len(m.metas))
	copy(out, m.metas)
	return out, nil
}

func (m *MemoryStore) Delete(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.sessions, id)
	m.reindexMetas()
	// Also remove associated events.
	filtered := m.events[:0]
	for _, e := range m.events {
		if e.SessionID != id {
			filtered = append(filtered, e)
		}
	}
	m.events = filtered
	return nil
}

func (m *MemoryStore) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	m.sessions = nil
	m.metas = nil
	m.events = nil
	m.requests = nil
	m.worktrees = nil
	return nil
}

func (m *MemoryStore) ReserveWorktree(_ context.Context, record worktree.Record) (worktree.Record, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if existing, ok := m.worktrees[record.ClientRequestID]; ok {
		return existing, false, nil
	}
	m.worktrees[record.ClientRequestID] = record
	return record, true, nil
}

func (m *MemoryStore) WorktreeByClientRequestID(_ context.Context, requestID string) (worktree.Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if record, ok := m.worktrees[requestID]; ok {
		return record, nil
	}
	return worktree.Record{}, worktree.ErrNotFound
}

func (m *MemoryStore) WorktreeBySessionID(_ context.Context, sessionID string) (worktree.Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, record := range m.worktrees {
		if record.SessionID == sessionID {
			return record, nil
		}
	}
	return worktree.Record{}, worktree.ErrNotFound
}

func (m *MemoryStore) ListWorktrees(_ context.Context) ([]worktree.Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]worktree.Record, 0, len(m.worktrees))
	for _, record := range m.worktrees {
		out = append(out, record)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

func (m *MemoryStore) UpdateWorktree(_ context.Context, record worktree.Record) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.worktrees[record.ClientRequestID]; !ok {
		return worktree.ErrNotFound
	}
	m.worktrees[record.ClientRequestID] = record
	return nil
}

var _ worktree.Store = (*MemoryStore)(nil)

func (m *MemoryStore) UpdateName(_ context.Context, id string, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	sess, ok := m.sessions[id]
	if !ok {
		return fmt.Errorf("session %q not found", id)
	}
	sess.Name = name
	sess.UpdatedAt = time.Now()
	m.reindexMetas()
	return nil
}

// ── EventStore ────────────────────────────────────────────────────────────

func (m *MemoryStore) RecordEvent(_ context.Context, e EventRecord) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	// A store-wide monotonic seq (1-based), mirroring the sqlite rowid so both
	// backends assign seqs the same way (v1.2 §4).
	m.eventSeq++
	e.Seq = m.eventSeq
	m.events = append(m.events, e)
	return e.Seq, nil
}

func (m *MemoryStore) SessionEvents(_ context.Context, sessionID string) ([]EventRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []EventRecord
	for _, e := range m.events {
		if e.SessionID == sessionID {
			out = append(out, e)
		}
	}
	return out, nil
}

func (m *MemoryStore) SessionEventsSince(_ context.Context, sessionID string, sinceSeq int64) ([]EventRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []EventRecord
	for _, e := range m.events {
		if e.SessionID == sessionID && e.Seq > sinceSeq {
			out = append(out, e)
		}
	}
	return out, nil
}

func (m *MemoryStore) RecentEventsByKind(_ context.Context, kind string, limit int) ([]EventRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []EventRecord
	// Walk in reverse (newest first).
	for i := len(m.events) - 1; i >= 0 && len(out) < limit; i-- {
		if m.events[i].Kind == kind {
			out = append(out, m.events[i])
		}
	}
	return out, nil
}

func (m *MemoryStore) SessionEventAttention(_ context.Context, sinceSequence int64) (EventAttentionSnapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	bySession := make(map[string]*EventAttention)
	for i := range m.events {
		e := m.events[i]
		head := bySession[e.SessionID]
		if head == nil {
			head = &EventAttention{SessionID: e.SessionID}
			bySession[e.SessionID] = head
		}
		if e.Seq > head.LastSequence {
			head.LastSequence = e.Seq
			latest := e
			head.LatestEvent = &latest
		}
		if isTerminalEventKind(e.Kind) && (head.LatestTerminal == nil || e.Seq > head.LatestTerminal.Seq) {
			terminal := e
			head.LatestTerminal = &terminal
		}
	}
	out := make([]EventAttention, 0, len(bySession))
	for _, head := range bySession {
		if head.LastSequence > sinceSequence {
			out = append(out, *head)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SessionID < out[j].SessionID })
	return EventAttentionSnapshot{LastSequence: m.eventSeq, Sessions: out}, nil
}

func isTerminalEventKind(kind string) bool {
	switch kind {
	case "turn_finished", "turn_failed", "turn_cancelled":
		return true
	default:
		return false
	}
}

// ── TelemetryStore ────────────────────────────────────────────────────────

func (m *MemoryStore) Stats(_ context.Context) (Stats, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	st := Stats{Sessions: len(m.sessions)}
	var sumBefore, sumAfter, sumSaved, sumRatio, sumChars float64
	var maxRatio, minRatio float64
	minRatio = math.MaxFloat64
	var maxPrompt int
	var maxPromptThreshold int

	for _, s := range m.sessions {
		if s.PromptTokens > maxPrompt {
			maxPrompt = s.PromptTokens
			maxPromptThreshold = s.CompactThreshold
		}
		for _, c := range s.Compactions {
			if c.AfterTokens >= 0 {
				st.Compactions++
				sumBefore += float64(c.BeforeTokens)
				sumAfter += float64(c.AfterTokens)
				sumSaved += float64(c.SavedTokens)
				sumRatio += c.CompressionRatio
				sumChars += float64(c.SummaryChars)
				if c.CompressionRatio > maxRatio {
					maxRatio = c.CompressionRatio
				}
				if c.CompressionRatio < minRatio {
					minRatio = c.CompressionRatio
				}
			}
		}
	}

	if st.Compactions > 0 {
		st.AvgBefore = sumBefore / float64(st.Compactions)
		st.AvgAfter = sumAfter / float64(st.Compactions)
		st.AvgSaved = sumSaved / float64(st.Compactions)
		st.AvgRatio = sumRatio / float64(st.Compactions)
		st.AvgSummaryChars = sumChars / float64(st.Compactions)
		st.MaxRatio = maxRatio
		st.MinRatio = minRatio
	}
	st.MaxPromptTokens = maxPrompt
	st.MaxCompactThreshold = maxPromptThreshold
	return st, nil
}

func (m *MemoryStore) RecordRequest(_ context.Context, r RequestRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.requests = append(m.requests, r)
	return nil
}

func (m *MemoryStore) ProviderStats(_ context.Context) (ProviderStats, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	var st ProviderStats
	var sumLatency float64
	var maxLatency int64
	var latencies []int64
	for _, r := range m.requests {
		st.Requests++
		if r.Success {
			st.Successes++
		} else {
			st.Failures++
		}
		if r.TimedOut {
			st.Timeouts++
		}
		st.Retries += r.Retries
		sumLatency += float64(r.LatencyMs)
		if r.LatencyMs > maxLatency {
			maxLatency = r.LatencyMs
		}
		latencies = append(latencies, r.LatencyMs)
	}
	if st.Requests > 0 {
		st.AvgLatencyMs = sumLatency / float64(st.Requests)
	}
	st.MaxLatencyMs = maxLatency
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	st.P50LatencyMs = Percentile(latencies, 50)
	st.P95LatencyMs = Percentile(latencies, 95)
	st.P99LatencyMs = Percentile(latencies, 99)
	st.Histogram = Histogram(latencies)
	return st, nil
}

func (m *MemoryStore) RecentRequests(_ context.Context, limit int) ([]RequestRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if limit <= 0 {
		limit = 20
	}
	n := len(m.requests)
	start := n - limit
	if start < 0 {
		start = 0
	}
	out := make([]RequestRecord, n-start)
	// Newest first.
	for i := n - 1; i >= start; i-- {
		out[n-1-i] = m.requests[i]
	}
	return out, nil
}

func (m *MemoryStore) TokenUsageByModel(_ context.Context) ([]ModelUsage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	agg := make(map[string]*ModelUsage)
	var order []string
	for _, r := range m.requests {
		u, ok := agg[r.Model]
		if !ok {
			order = append(order, r.Model)
			u = &ModelUsage{Model: r.Model}
			agg[r.Model] = u
		}
		u.Requests++
		u.PromptTokens += int64(r.PromptTokens)
		u.CachedPromptTokens += int64(r.CachedPromptTokens)
		u.CompletionTokens += int64(r.CompletionTokens)
	}
	// Sort by total tokens descending (matching SQLiteStore ordering).
	sort.Slice(order, func(i, j int) bool {
		ui, uj := agg[order[i]], agg[order[j]]
		return ui.PromptTokens+ui.CompletionTokens > uj.PromptTokens+uj.CompletionTokens
	})
	out := make([]ModelUsage, len(order))
	for i, m := range order {
		out[i] = *agg[m]
	}
	return out, nil
}

// ── Helpers ───────────────────────────────────────────────────────────────

// reindexMetas rebuilds the metas slice from the sessions map. The caller must
// hold m.mu. Called after every mutation that changes the session listing.
func (m *MemoryStore) reindexMetas() {
	m.metas = make([]Meta, 0, len(m.sessions))
	for _, s := range m.sessions {
		title := ""
		if len(s.Messages) > 1 {
			title = s.Messages[1].Content // Messages[0] is always the system prompt.
		}
		// Truncate title for display.
		if len(title) > 80 {
			title = title[:80]
		}

		var compactions int
		var totalSaved int
		var lastCompacted time.Time
		for _, c := range s.Compactions {
			if c.AfterTokens >= 0 {
				compactions++
				totalSaved += c.SavedTokens
				if c.CompactedAt.After(lastCompacted) {
					lastCompacted = c.CompactedAt
				}
			}
		}

		m.metas = append(m.metas, Meta{
			ID:            s.ID,
			Name:          s.Name,
			Title:         title,
			Model:         s.Model,
			MessageCount:  len(s.Messages),
			PromptTokens:  s.PromptTokens,
			UpdatedAt:     s.UpdatedAt,
			WorkspacePath: s.WorkspacePath,
			Compactions:   compactions,
			TotalSaved:    totalSaved,
			LastCompacted: lastCompacted,
		})
	}
	sort.Slice(m.metas, func(i, j int) bool {
		return m.metas[i].UpdatedAt.After(m.metas[j].UpdatedAt)
	})
}

// deepCopySession returns an independent copy of s. Messages and their tool
// calls are deep-copied; the metadata map is shallow-copied (values are opaque
// to the store). This prevents a caller from mutating stored state through a
// previously-saved reference.
func deepCopySession(s *Session) *Session {
	if s == nil {
		return nil
	}
	c := *s
	c.Messages = make([]model.Message, len(s.Messages))
	for i, m := range s.Messages {
		c.Messages[i] = m
		if len(m.ToolCalls) > 0 {
			tc := make([]model.ToolCall, len(m.ToolCalls))
			copy(tc, m.ToolCalls)
			c.Messages[i].ToolCalls = tc
		}
	}
	c.Metadata = make(map[string]any, len(s.Metadata))
	for k, v := range s.Metadata {
		c.Metadata[k] = v
	}
	c.Compactions = make([]CompactionStats, len(s.Compactions))
	copy(c.Compactions, s.Compactions)
	return &c
}
