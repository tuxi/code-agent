package sqlite

import (
	"context"
	"path/filepath"
	"testing"

	"code-agent/internal/session"
)

func TestLegacyConversationPreviewMigration(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "legacy.db")
	store, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	sess := sampleSession()
	want := sess.ConversationPreview()
	if want == "" {
		t.Fatal("fixture needs a first message")
	}
	if err = store.Save(ctx, sess); err != nil {
		t.Fatal(err)
	}
	if _, err = store.db.Exec(`ALTER TABLE sessions DROP COLUMN conversation_preview`); err != nil {
		t.Fatal(err)
	}
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = New(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	rows, _, err := store.ListSessionPage(ctx, false, nil, 10)
	if err != nil || len(rows) != 1 || rows[0].Preview != want {
		t.Fatalf("list=%+v want preview=%q err=%v", rows, want, err)
	}
}

func TestLoadHeaderDoesNotReadTranscriptTables(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	sess := sampleSession()
	sess.Name = "header name"
	sess.Metadata = map[string]any{session.MetaForkSource: "source", session.MetaTurnStatus: "paused"}
	if err := store.Save(ctx, sess); err != nil {
		t.Fatal(err)
	}
	// A header lookup must work even when transcript tables are unavailable.
	if _, err := store.db.Exec(`DROP TABLE messages`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`DROP TABLE compactions`); err != nil {
		t.Fatal(err)
	}
	header, err := store.LoadHeader(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if header.ID != sess.ID || header.Name != sess.Name || header.TurnStatus() != "paused" || header.Metadata[session.MetaForkSource] != "source" {
		t.Fatalf("header=%+v", header)
	}
	if len(header.Messages) != 0 || len(header.Compactions) != 0 || header.Summary != "" || len(header.ReferenceLedger) != 0 {
		t.Fatal("header contains transcript data")
	}
	if _, err := store.Load(ctx, sess.ID); err == nil {
		t.Fatal("full Load should still require transcript tables")
	}
	if _, err := store.LoadHeader(ctx, "missing"); err == nil {
		t.Fatal("unknown header should fail")
	}
}
