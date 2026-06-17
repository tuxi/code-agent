package session

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"code-agent/internal/model"
)

func newStore(t *testing.T) *SQLiteStore {
	t.Helper()
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "sessions.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

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

func TestSQLiteStoreRoundTrip(t *testing.T) {
	store := newStore(t)
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
	// tool_calls and tool_call_id must survive — without them the resumed history
	// is invalid to send back to the model.
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

func TestSQLiteStoreSaveIsSnapshot(t *testing.T) {
	store := newStore(t)
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

func TestSQLiteStoreListAndDelete(t *testing.T) {
	store := newStore(t)
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

func TestSQLiteStoreLoadMissing(t *testing.T) {
	store := newStore(t)
	if _, err := store.Load(context.Background(), "nope"); err == nil {
		t.Fatal("expected an error loading a missing session")
	}
}
