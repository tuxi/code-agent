package session

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"code-agent/internal/model"
)

func sampleSession() *Session {
	now := time.Now().Truncate(time.Millisecond)
	return &Session{
		ID:      "20260616-101500-deadbeef",
		Model:   "glm-5.1",
		Summary: "DIGEST",
		Messages: []model.Message{
			{Role: model.RoleSystem, Content: "sys"},
			{Role: model.RoleUser, Content: "look at loop.go"},
			{Role: model.RoleAssistant, ToolCalls: []model.ToolCall{
				{ID: "a", Type: "function", Function: model.FunctionCall{Name: "read_file", Arguments: `{"path":"loop.go"}`}},
			}},
			{Role: model.RoleTool, ToolCallID: "a", Content: "package agent"},
			{Role: model.RoleAssistant, Content: "it drives the loop"},
		},
		Compactions: []CompactionStats{
			{BeforeTokens: 90000, AfterTokens: 27000, SavedTokens: 63000, CompressionRatio: 0.7, SummaryChars: 1800, CompactedAt: now},
		},
		PromptTokens:     27000,
		ContextWindow:    128000,
		CompactThreshold: 89600,
		CreatedAt:        now,
		UpdatedAt:        now,
	}
}

func newMemStore(t *testing.T) *MemoryStore {
	t.Helper()
	s := NewMemoryStore()
	t.Cleanup(func() { s.Close() })
	return s
}

func TestMemoryStoreRoundTrip(t *testing.T) {
	store := newMemStore(t)
	ctx := context.Background()
	in := sampleSession()

	if err := store.Save(ctx, in); err != nil {
		t.Fatalf("save: %v", err)
	}

	got, err := store.Load(ctx, in.ID)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if got.Model != "glm-5.1" || got.Summary != "DIGEST" {
		t.Fatalf("scalar fields lost: model=%q summary=%q", got.Model, got.Summary)
	}
	if got.PromptTokens != 27000 || got.ContextWindow != 128000 || got.CompactThreshold != 89600 {
		t.Fatalf("budget lost: %+v", got)
	}
	if len(got.Messages) != 5 {
		t.Fatalf("messages = %d, want 5", len(got.Messages))
	}
	if len(got.Messages[2].ToolCalls) != 1 || got.Messages[2].ToolCalls[0].Function.Name != "read_file" {
		t.Fatalf("tool_calls lost: %+v", got.Messages[2])
	}
	if got.Messages[3].ToolCallID != "a" {
		t.Fatalf("tool_call_id lost: %+v", got.Messages[3])
	}
	assertValidSequence(t, got.Messages)

	if len(got.Compactions) != 1 || got.Compactions[0].SavedTokens != 63000 {
		t.Fatalf("compaction trace lost: %+v", got.Compactions)
	}
}

func TestMemoryStoreSaveIsSnapshot(t *testing.T) {
	store := newMemStore(t)
	ctx := context.Background()
	sess := sampleSession()

	if err := store.Save(ctx, sess); err != nil {
		t.Fatal(err)
	}
	// Re-saving must replace, not append (no duplicate rows).
	sess.Messages = append(sess.Messages, model.Message{Role: model.RoleUser, Content: "next"})
	if err := store.Save(ctx, sess); err != nil {
		t.Fatal(err)
	}
	got, err := store.Load(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Messages) != 6 {
		t.Fatalf("re-save should snapshot, got %d messages, want 6", len(got.Messages))
	}
}

func TestMemoryStoreListAndDelete(t *testing.T) {
	store := newMemStore(t)
	ctx := context.Background()
	sess := sampleSession()
	if err := store.Save(ctx, sess); err != nil {
		t.Fatal(err)
	}

	metas, err := store.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(metas) != 1 || metas[0].ID != sess.ID || metas[0].Model != "glm-5.1" || metas[0].MessageCount != 5 {
		t.Fatalf("list wrong: %+v", metas)
	}

	if err := store.Delete(ctx, sess.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(ctx, sess.ID); err == nil {
		t.Fatal("expected an error loading a deleted session")
	}
	metas, _ = store.List(ctx)
	if len(metas) != 0 {
		t.Fatalf("expected no sessions after delete, got %d", len(metas))
	}
}

func TestMemoryStoreListOrder(t *testing.T) {
	store := newMemStore(t)
	ctx := context.Background()

	old := sampleSession()
	old.ID = "older"
	old.UpdatedAt = time.Now().Add(-time.Hour)
	newer := sampleSession()
	newer.ID = "newer"
	newer.UpdatedAt = time.Now()

	if err := store.Save(ctx, old); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(ctx, newer); err != nil {
		t.Fatal(err)
	}

	metas, err := store.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(metas) != 2 {
		t.Fatalf("expected 2 sessions, got %d", len(metas))
	}
	// Newest first.
	if metas[0].ID != "newer" {
		t.Fatalf("first should be 'newer', got %q", metas[0].ID)
	}
	if metas[1].ID != "older" {
		t.Fatalf("second should be 'older', got %q", metas[1].ID)
	}
}

func TestMemoryStoreStats(t *testing.T) {
	store := newMemStore(t)
	ctx := context.Background()

	s1 := sampleSession()
	s1.ID = "s1"
	s1.Compactions = []CompactionStats{
		{BeforeTokens: 90000, AfterTokens: 30000, SavedTokens: 60000, CompressionRatio: 0.60, SummaryChars: 1000, CompactedAt: time.Now()},
		{BeforeTokens: 80000, AfterTokens: 20000, SavedTokens: 60000, CompressionRatio: 0.75, SummaryChars: 2000, CompactedAt: time.Now()},
		{BeforeTokens: 50000, AfterTokens: -1}, // pending — must be excluded
	}
	if err := store.Save(ctx, s1); err != nil {
		t.Fatal(err)
	}
	s2 := sampleSession()
	s2.ID = "s2"
	s2.Compactions = nil
	if err := store.Save(ctx, s2); err != nil {
		t.Fatal(err)
	}

	st, err := store.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Sessions != 2 {
		t.Fatalf("sessions = %d, want 2", st.Sessions)
	}
	if st.Compactions != 2 {
		t.Fatalf("compactions = %d, want 2 (pending excluded)", st.Compactions)
	}
	if st.AvgBefore != 85000 || st.AvgAfter != 25000 || st.AvgSaved != 60000 {
		t.Fatalf("avg before/after/saved = %.0f/%.0f/%.0f, want 85000/25000/60000",
			st.AvgBefore, st.AvgAfter, st.AvgSaved)
	}
	if st.AvgRatio < 0.674 || st.AvgRatio > 0.676 {
		t.Fatalf("avg ratio = %v, want ~0.675", st.AvgRatio)
	}
	if st.MaxRatio != 0.75 || st.MinRatio != 0.60 {
		t.Fatalf("max/min ratio = %v/%v, want 0.75/0.60", st.MaxRatio, st.MinRatio)
	}
}

func TestMemoryStoreStatsEmpty(t *testing.T) {
	st, err := newMemStore(t).Stats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.Sessions != 0 || st.Compactions != 0 || st.AvgRatio != 0 {
		t.Fatalf("empty store should yield zeros, got %+v", st)
	}
}

func TestMemoryStoreListIncludesCompactionAggregates(t *testing.T) {
	store := newMemStore(t)
	ctx := context.Background()
	s := sampleSession()
	s.Compactions = []CompactionStats{
		{BeforeTokens: 90000, AfterTokens: 30000, SavedTokens: 60000, CompressionRatio: 0.6, SummaryChars: 1000, CompactedAt: time.Now()},
		{BeforeTokens: 80000, AfterTokens: 25000, SavedTokens: 55000, CompressionRatio: 0.68, SummaryChars: 1500, CompactedAt: time.Now()},
	}
	if err := store.Save(ctx, s); err != nil {
		t.Fatal(err)
	}

	metas, err := store.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(metas) != 1 {
		t.Fatalf("expected 1 session, got %d", len(metas))
	}
	if metas[0].Compactions != 2 {
		t.Fatalf("compactions = %d, want 2", metas[0].Compactions)
	}
	if metas[0].TotalSaved != 115000 {
		t.Fatalf("total saved = %d, want 115000", metas[0].TotalSaved)
	}
	if metas[0].LastCompacted.IsZero() {
		t.Fatal("expected a non-zero last-compacted time")
	}
}

func TestMemoryStoreEventRoundTrip(t *testing.T) {
	store := newMemStore(t)
	ctx := context.Background()
	now := time.Now().Truncate(time.Millisecond)

	events := []EventRecord{
		{SessionID: "s1", TurnID: "t1", Kind: "turn_started", At: now, Payload: json.RawMessage(`{"text":"fix it"}`)},
		{SessionID: "s1", TurnID: "t1", Kind: "tool_started", At: now.Add(time.Second), Payload: json.RawMessage(`{"tool":"grep"}`)},
		{SessionID: "s1", TurnID: "t1", Kind: "tool_finished", At: now.Add(2 * time.Second), Payload: json.RawMessage(`{"ok":true}`)},
		{SessionID: "s2", TurnID: "t9", Kind: "turn_started", At: now, Payload: json.RawMessage(`{"text":"other session"}`)},
	}
	for _, e := range events {
		if err := store.RecordEvent(ctx, e); err != nil {
			t.Fatalf("RecordEvent: %v", err)
		}
	}

	got, err := store.SessionEvents(ctx, "s1")
	if err != nil {
		t.Fatalf("SessionEvents: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("want 3 events for s1 (s2's event must not leak), got %d", len(got))
	}
	if got[0].Kind != "turn_started" || got[1].Kind != "tool_started" || got[2].Kind != "tool_finished" {
		t.Fatalf("events out of order: %v %v %v", got[0].Kind, got[1].Kind, got[2].Kind)
	}
	if got[0].TurnID != "t1" || string(got[1].Payload) != `{"tool":"grep"}` {
		t.Fatalf("fields not round-tripped: %+v", got[1])
	}
	if !got[2].At.Equal(now.Add(2 * time.Second)) {
		t.Fatalf("timestamp not round-tripped: %v", got[2].At)
	}
}

func TestMemoryStoreDeleteRemovesEvents(t *testing.T) {
	store := newMemStore(t)
	ctx := context.Background()
	if err := store.RecordEvent(ctx, EventRecord{SessionID: "doomed", Kind: "thinking", At: time.Now()}); err != nil {
		t.Fatalf("RecordEvent: %v", err)
	}
	if err := store.Delete(ctx, "doomed"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	got, err := store.SessionEvents(ctx, "doomed")
	if err != nil {
		t.Fatalf("SessionEvents: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("Delete should have removed the session's events, got %d", len(got))
	}
}

func TestMemoryStoreRecentEventsByKind(t *testing.T) {
	store := newMemStore(t)
	ctx := context.Background()
	now := time.Now()

	// Record 3 events of "task_started" across different sessions.
	for i, sid := range []string{"s1", "s2", "s3"} {
		if err := store.RecordEvent(ctx, EventRecord{
			SessionID: sid, Kind: "task_started", At: now.Add(time.Duration(i)*time.Second),
		}); err != nil {
			t.Fatal(err)
		}
	}
	// Record a different kind — must not appear in the result.
	if err := store.RecordEvent(ctx, EventRecord{
		SessionID: "s4", Kind: "thinking", At: now,
	}); err != nil {
		t.Fatal(err)
	}

	got, err := store.RecentEventsByKind(ctx, "task_started", 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("want 3 task_started events, got %d", len(got))
	}
	// Newest first (s3, s2, s1).
	if got[0].SessionID != "s3" || got[1].SessionID != "s2" || got[2].SessionID != "s1" {
		t.Fatalf("wrong order: %v %v %v", got[0].SessionID, got[1].SessionID, got[2].SessionID)
	}
}

func TestMemoryStoreProviderStats(t *testing.T) {
	store := newMemStore(t)
	ctx := context.Background()
	recs := []RequestRecord{
		{Model: "m", Success: true, Attempts: 1, Retries: 0, LatencyMs: 1000, At: time.Now()},
		{Model: "m", Success: true, Attempts: 2, Retries: 1, TimedOut: true, LatencyMs: 31000, At: time.Now()},
		{Model: "m", Success: false, Attempts: 3, Retries: 2, TimedOut: true, ErrorClass: "timeout", LatencyMs: 90000, At: time.Now()},
	}
	for _, r := range recs {
		if err := store.RecordRequest(ctx, r); err != nil {
			t.Fatal(err)
		}
	}

	st, err := store.ProviderStats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Requests != 3 || st.Successes != 2 || st.Failures != 1 {
		t.Fatalf("requests/successes/failures = %d/%d/%d, want 3/2/1", st.Requests, st.Successes, st.Failures)
	}
	if st.Timeouts != 2 {
		t.Fatalf("timeouts = %d, want 2", st.Timeouts)
	}
	if st.Retries != 3 {
		t.Fatalf("retries = %d, want 3 (0+1+2)", st.Retries)
	}
	if st.MaxLatencyMs != 90000 {
		t.Fatalf("max latency = %d, want 90000", st.MaxLatencyMs)
	}
}

func TestMemoryStoreRecentRequestsTrace(t *testing.T) {
	store := newMemStore(t)
	ctx := context.Background()
	if err := store.RecordRequest(ctx, RequestRecord{
		Model: "m", Success: true, Attempts: 1, LatencyMs: 5000, At: time.Now().Add(-time.Minute),
		Trace: []AttemptRecord{{LatencyMs: 5000, Result: "success"}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordRequest(ctx, RequestRecord{
		Model: "m", Success: true, Attempts: 2, Retries: 1, TimedOut: true, LatencyMs: 35000, At: time.Now(),
		Trace: []AttemptRecord{{LatencyMs: 30000, Result: "timeout"}, {LatencyMs: 5000, Result: "success"}},
	}); err != nil {
		t.Fatal(err)
	}

	recs, err := store.RecentRequests(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 2 {
		t.Fatalf("want 2 requests, got %d", len(recs))
	}
	// Newest first, with the per-attempt trace preserved.
	if recs[0].Attempts != 2 || len(recs[0].Trace) != 2 {
		t.Fatalf("newest-first/trace wrong: %+v", recs[0])
	}
	if recs[0].Trace[0].Result != "timeout" || recs[0].Trace[0].LatencyMs != 30000 {
		t.Fatalf("trace[0] wrong: %+v", recs[0].Trace[0])
	}
	if recs[0].Trace[1].Result != "success" {
		t.Fatalf("trace[1] wrong: %+v", recs[0].Trace[1])
	}

	if one, _ := store.RecentRequests(ctx, 1); len(one) != 1 {
		t.Fatalf("limit not applied: got %d", len(one))
	}
}

func TestMemoryStoreTokenUsageByModel(t *testing.T) {
	store := newMemStore(t)
	ctx := context.Background()
	recs := []RequestRecord{
		{Model: "glm-5.1", PromptTokens: 1000, CompletionTokens: 200, Success: true, At: time.Now()},
		{Model: "glm-5.1", PromptTokens: 3000, CompletionTokens: 500, Success: true, At: time.Now()},
		{Model: "deepseek-v4", PromptTokens: 500, CompletionTokens: 100, Success: true, At: time.Now()},
	}
	for _, r := range recs {
		if err := store.RecordRequest(ctx, r); err != nil {
			t.Fatal(err)
		}
	}

	usage, err := store.TokenUsageByModel(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(usage) != 2 {
		t.Fatalf("want 2 models, got %d", len(usage))
	}
	// Ordered by total tokens desc: glm (4700) before deepseek (600).
	if usage[0].Model != "glm-5.1" || usage[0].Requests != 2 ||
		usage[0].PromptTokens != 4000 || usage[0].CompletionTokens != 700 {
		t.Fatalf("glm usage wrong: %+v", usage[0])
	}
	if usage[1].Model != "deepseek-v4" || usage[1].PromptTokens != 500 || usage[1].CompletionTokens != 100 {
		t.Fatalf("deepseek usage wrong: %+v", usage[1])
	}
}

func TestMemoryStoreProviderStatsEmpty(t *testing.T) {
	st, err := newMemStore(t).ProviderStats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.Requests != 0 || st.MaxLatencyMs != 0 {
		t.Fatalf("empty store should be zero: %+v", st)
	}
}

func TestMemoryStoreLoadMissing(t *testing.T) {
	store := newMemStore(t)
	if _, err := store.Load(context.Background(), "nope"); err == nil {
		t.Fatal("expected an error loading a missing session")
	}
}

func TestMemoryStoreDeepCopyIsolation(t *testing.T) {
	store := newMemStore(t)
	ctx := context.Background()
	sess := sampleSession()

	if err := store.Save(ctx, sess); err != nil {
		t.Fatal(err)
	}
	// Mutate the original — must not affect the stored copy.
	sess.Messages[1].Content = "corrupted"
	if err := store.Save(ctx, sess); err != nil {
		t.Fatal(err)
	}

	got, err := store.Load(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	// The loaded copy should still have the original content from sampleSession,
	// NOT "corrupted", because deepCopySession was called on the first Save.
	// Actually, the second Save overwrites with "corrupted", so after Load we
	// expect "corrupted". The real test: load again, mutate the loaded result,
	// and verify the store is unchanged.
	if got.Messages[1].Content != "corrupted" {
		t.Fatalf("content should be 'corrupted' after second save, got %q", got.Messages[1].Content)
	}

	// Load and mutate the returned copy — store must be unaffected.
	got2, err := store.Load(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	got2.Messages[1].Content = "mutated after load"

	got3, err := store.Load(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got3.Messages[1].Content != "corrupted" {
		t.Fatalf("store was corrupted by mutating a loaded copy: got %q, want 'corrupted'", got3.Messages[1].Content)
	}
}

func TestMemoryStoreLatencyPercentilesAndHistogram(t *testing.T) {
	store := newMemStore(t)
	ctx := context.Background()
	for i := 1; i <= 10; i++ {
		if err := store.RecordRequest(ctx, RequestRecord{
			Model: "m", Success: true, Attempts: 1, LatencyMs: int64(i * 1000), At: time.Now(),
		}); err != nil {
			t.Fatal(err)
		}
	}

	st, err := store.ProviderStats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.P50LatencyMs != 5000 || st.P95LatencyMs != 10000 || st.P99LatencyMs != 10000 {
		t.Fatalf("P50/P95/P99 = %d/%d/%d, want 5000/10000/10000",
			st.P50LatencyMs, st.P95LatencyMs, st.P99LatencyMs)
	}

	counts := map[string]int{}
	total := 0
	for _, b := range st.Histogram {
		counts[b.Label] = b.Count
		total += b.Count
	}
	if total != 10 {
		t.Fatalf("histogram total = %d, want 10 (all requests bucketed)", total)
	}
	if counts["1-2s"] != 1 || counts["2-5s"] != 3 || counts["5-10s"] != 5 || counts["10-20s"] != 1 {
		t.Fatalf("histogram counts wrong: %+v", counts)
	}
}

func TestMemoryStoreClosed(t *testing.T) {
	store := NewMemoryStore()
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	// Save after close must error.
	if err := store.Save(context.Background(), sampleSession()); err == nil {
		t.Fatal("expected error saving to closed store")
	}
	// Close must be idempotent.
	if err := store.Close(); err != nil {
		t.Fatalf("second close must not error: %v", err)
	}
}
