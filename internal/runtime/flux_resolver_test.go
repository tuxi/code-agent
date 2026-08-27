package runtime

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/tuxi/flux-workflow/domain"

	"code-agent/internal/model"
	"code-agent/internal/session"
	sessionsqlite "code-agent/internal/session/sqlite"
)

// TestFluxResolverUsesIndexStorePath verifies the external resolver reads the
// session's events from the authoritative store_path recorded in the index,
// not a workspace-derived store. A session routed to another process's store
// (supervisor/owner routing) has its events only under store_path; reading the
// workspace-derived store would miss turn_finished and stall the binding.
func TestFluxResolverUsesIndexStorePath(t *testing.T) {
	base := useIndexBaseDir(t)
	ctx := context.Background()
	now := time.Now().UTC()
	workspaceRoot := t.TempDir() // workspace-derived store would be empty

	// The session's real store (store_path): events live here, NOT in a
	// workspace-derived store.
	sourcePath := filepath.Join(base, "source.db")
	source, err := sessionsqlite.New(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	sess := &session.Session{
		ID: "routed-session", Name: "routed", Model: "m",
		WorkspacePath: workspaceRoot,
		CreatedAt:     now, UpdatedAt: now,
		Messages: []model.Message{{Role: model.RoleUser, Content: "hi"}},
	}
	if err := source.Save(ctx, sess); err != nil {
		t.Fatal(err)
	}
	if _, err := source.RecordEvent(ctx, session.EventRecord{SessionID: sess.ID, TurnID: "turn_r", Kind: "turn_started", At: now}); err != nil {
		t.Fatal(err)
	}
	if _, err := source.RecordEvent(ctx, session.EventRecord{SessionID: sess.ID, TurnID: "turn_r", Kind: "turn_finished", At: now}); err != nil {
		t.Fatal(err)
	}
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}

	db, err := OpenIndex()
	if err != nil {
		t.Fatal(err)
	}
	WriteSessionIndex(db, sess, sourcePath)

	r := &externalResolver{}
	payload, err := r.ResolveAwait(ctx, &domain.AwaitBinding{Correlation: map[string]any{
		"session_id": sess.ID, "turn_id": "turn_r", "cursor": "0",
	}})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if payload == nil || payload["status"] != "completed" {
		t.Fatalf("resolver did not complete from store_path store: %v", payload)
	}
}

// TestFluxResolverStillRunningNoTerminal verifies the resolver reports "still
// running" (nil, nil) when the turn has no terminal event yet.
func TestFluxResolverStillRunningNoTerminal(t *testing.T) {
	base := useIndexBaseDir(t)
	ctx := context.Background()
	now := time.Now().UTC()

	sourcePath := filepath.Join(base, "source2.db")
	source, err := sessionsqlite.New(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	sess := &session.Session{ID: "running-session", Name: "r", Model: "m", CreatedAt: now, UpdatedAt: now}
	if err := source.Save(ctx, sess); err != nil {
		t.Fatal(err)
	}
	if _, err := source.RecordEvent(ctx, session.EventRecord{SessionID: sess.ID, TurnID: "turn_r", Kind: "turn_started", At: now}); err != nil {
		t.Fatal(err)
	}
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}

	db, err := OpenIndex()
	if err != nil {
		t.Fatal(err)
	}
	WriteSessionIndex(db, sess, sourcePath)

	r := &externalResolver{}
	payload, err := r.ResolveAwait(ctx, &domain.AwaitBinding{Correlation: map[string]any{
		"session_id": sess.ID, "turn_id": "turn_r", "cursor": "0",
	}})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if payload != nil {
		t.Fatalf("expected still running, got %v", payload)
	}
}
