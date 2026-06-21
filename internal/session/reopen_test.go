package session

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"code-agent/internal/model"
)

// After a reopen (the recovery path for SQLITE_READONLY_DBMOVED) the store must
// still write and read correctly, and the wholesale Save must preserve the full
// session — proving the retry loses nothing.
func TestReopenRoundTrip(t *testing.T) {
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	sess := &Session{ID: "x", Messages: []model.Message{{Role: model.RoleUser, Content: "hi"}}}
	if err := store.Save(context.Background(), sess); err != nil {
		t.Fatal(err)
	}

	if err := store.reopen(); err != nil {
		t.Fatalf("reopen: %v", err)
	}

	sess.Messages = append(sess.Messages, model.Message{Role: model.RoleAssistant, Content: "yo"})
	if err := store.Save(context.Background(), sess); err != nil {
		t.Fatalf("save after reopen: %v", err)
	}

	loaded, err := store.Load(context.Background(), "x")
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Messages) != 2 {
		t.Errorf("messages after reopen = %d, want 2", len(loaded.Messages))
	}
}

func TestIsReadonlyErr(t *testing.T) {
	if !isReadonlyErr(errors.New("attempt to write a readonly database (1032)")) {
		t.Error("should match the READONLY_DBMOVED message")
	}
	if isReadonlyErr(errors.New("some other failure")) {
		t.Error("should not match an unrelated error")
	}
	if isReadonlyErr(nil) {
		t.Error("nil must be false")
	}
}
