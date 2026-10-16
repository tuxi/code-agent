package runtime

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"code-agent/internal/session"
	sessionsqlite "code-agent/internal/session/sqlite"
)

func TestServerOpenDefersColdIndexWithoutLosingOldSessions(t *testing.T) {
	base := useIndexBaseDir(t)
	ctx := context.Background()
	source, err := sessionsqlite.New(filepath.Join(base, "sessions.db"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := source.Save(ctx, &session.Session{ID: "old", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	source.Close()
	store, err := OpenStoreForServer("test")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var count int
	if err := IndexDB().QueryRow(`SELECT COUNT(*) FROM sessions`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("index was synchronously rebuilt: count=%d err=%v", count, err)
	}
	// A new save before the first tool query must not hide pre-existing rows.
	if err := store.Save(ctx, &session.Session{ID: "new", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	entries, err := SessionIndex().ListAll()
	if err != nil || len(entries) != 2 {
		t.Fatalf("entries=%+v err=%v", entries, err)
	}
}
