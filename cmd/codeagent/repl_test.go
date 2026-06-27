package main

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"code-agent/internal/app"
	"code-agent/internal/model"
	"code-agent/internal/session"
	"code-agent/internal/session/sqlite"
)

func testStore(t *testing.T) session.Store {
	t.Helper()
	store, err := sqlite.New(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func seedSession(t *testing.T, store session.Store, id string, updated time.Time) {
	t.Helper()
	s := &session.Session{
		ID:    id,
		Model: "old-model",
		Messages: []model.Message{
			{Role: model.RoleSystem, Content: "sys"},
			{Role: model.RoleUser, Content: "hi"},
		},
		CreatedAt: updated,
		UpdatedAt: updated,
	}
	if err := store.Save(context.Background(), s); err != nil {
		t.Fatalf("seed %s: %v", id, err)
	}
}

func testConfigModel() (app.Config, *app.ModelConfig) {
	cfg := app.Config{Agent: app.AgentConfig{CompactRatio: 0.7}}
	mc := &app.ModelConfig{Name: "m", Model: "wire-model", ContextWindow: 128000}
	return cfg, mc
}

// promptReturning is a lineReader that always returns s, standing in for the
// readline-backed reader the REPL uses.
func promptReturning(s string) lineReader {
	return func(string) (string, error) { return s, nil }
}

// Interactive: list, pick a number, switch — and re-budget to the current model.
func TestResumeInteractiveSelect(t *testing.T) {
	store := testStore(t)
	now := time.Now()
	seedSession(t, store, "sess-a", now.Add(-time.Hour)) // older
	seedSession(t, store, "sess-b", now)                 // newer => listed first

	cfg, mc := testConfigModel()
	current := &session.Session{ID: "sess-a", Model: "old-model"}

	// List orders newest-first: [1]=sess-b, [2]=sess-a. Pick 1 => sess-b.
	next, err := resumeInteractive(cfg, mc, current, store, promptReturning("1"), []string{"/resume"})
	if err != nil {
		t.Fatal(err)
	}
	if next.ID != "sess-b" {
		t.Fatalf("switched to %q, want sess-b", next.ID)
	}
	// Re-budgeted to the current model, not the stored one.
	if next.Model != "wire-model" || next.CompactThreshold != 89600 || next.ContextWindow != 128000 {
		t.Fatalf("not re-budgeted to current model: %+v", next)
	}
}

func TestResumeCancelKeepsCurrent(t *testing.T) {
	store := testStore(t)
	seedSession(t, store, "sess-b", time.Now())
	cfg, mc := testConfigModel()
	current := &session.Session{ID: "sess-a"}

	next, err := resumeInteractive(cfg, mc, current, store, promptReturning(""), []string{"/resume"})
	if err != nil {
		t.Fatal(err)
	}
	if next.ID != "sess-a" {
		t.Fatalf("cancel must keep current, switched to %q", next.ID)
	}
}

func TestResumeDirectByID(t *testing.T) {
	store := testStore(t)
	seedSession(t, store, "sess-b", time.Now())
	cfg, mc := testConfigModel()
	current := &session.Session{ID: "sess-a"}

	next, err := resumeInteractive(cfg, mc, current, store, promptReturning(""), []string{"/resume", "sess-b"})
	if err != nil {
		t.Fatal(err)
	}
	if next.ID != "sess-b" {
		t.Fatalf("direct resume switched to %q, want sess-b", next.ID)
	}
}

// An untouched session (only the system prompt) must not be saved when resuming
// away from it — otherwise every launch+/resume leaks an empty msgs=1 session.
func TestResumeDoesNotPersistEmptyCurrent(t *testing.T) {
	store := testStore(t)
	seedSession(t, store, "sess-b", time.Now())
	cfg, mc := testConfigModel()
	current := &session.Session{
		ID:       "sess-fresh",
		Messages: []model.Message{{Role: model.RoleSystem, Content: "sys"}},
	}

	next, err := resumeInteractive(cfg, mc, current, store, promptReturning(""), []string{"/resume", "sess-b"})
	if err != nil {
		t.Fatal(err)
	}
	if next.ID != "sess-b" {
		t.Fatalf("should switch to sess-b, got %q", next.ID)
	}
	metas, _ := store.List(context.Background())
	for _, m := range metas {
		if m.ID == "sess-fresh" {
			t.Fatal("an untouched session must not be persisted on resume")
		}
	}
}

// A session with real turns IS saved before switching away.
func TestResumePersistsNonEmptyCurrent(t *testing.T) {
	store := testStore(t)
	seedSession(t, store, "sess-b", time.Now())
	cfg, mc := testConfigModel()
	current := &session.Session{
		ID: "sess-cur",
		Messages: []model.Message{
			{Role: model.RoleSystem, Content: "sys"},
			{Role: model.RoleUser, Content: "real work"},
			{Role: model.RoleAssistant, Content: "done"},
		},
	}

	if _, err := resumeInteractive(cfg, mc, current, store, promptReturning(""), []string{"/resume", "sess-b"}); err != nil {
		t.Fatal(err)
	}
	metas, _ := store.List(context.Background())
	var found bool
	for _, m := range metas {
		if m.ID == "sess-cur" {
			found = true
		}
	}
	if !found {
		t.Fatal("a session with real turns must be persisted on resume")
	}
}

func TestResumeInvalidSelection(t *testing.T) {
	store := testStore(t)
	seedSession(t, store, "sess-b", time.Now())
	cfg, mc := testConfigModel()
	current := &session.Session{ID: "sess-a"}

	next, err := resumeInteractive(cfg, mc, current, store, promptReturning("99"), []string{"/resume"})
	if err == nil {
		t.Fatal("expected an error for an out-of-range selection")
	}
	if next.ID != "sess-a" {
		t.Fatalf("invalid selection must keep current, got %q", next.ID)
	}
}
