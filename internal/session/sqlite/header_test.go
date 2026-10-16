package sqlite

import (
	"context"
	"testing"

	"code-agent/internal/session"
)

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
