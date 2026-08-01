package runtime

import (
	"context"
	"database/sql"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"code-agent/internal/model"
	"code-agent/internal/session"
	sessionsqlite "code-agent/internal/session/sqlite"
)

func useIndexBaseDir(t *testing.T) string {
	t.Helper()
	oldBase := StoreBaseDir()
	base := t.TempDir()
	SetStoreBaseDir(base)
	t.Cleanup(func() { SetStoreBaseDir(oldBase) })
	return base
}

func TestOpenIndexIsSingletonWithConcurrentPragmas(t *testing.T) {
	useIndexBaseDir(t)

	db, err := EnsureIndexReady(context.Background())
	if err != nil {
		t.Fatalf("EnsureIndexReady: %v", err)
	}
	other, err := OpenIndex()
	if err != nil {
		t.Fatalf("OpenIndex: %v", err)
	}
	if other != db {
		t.Fatal("OpenIndex returned a second process-local handle")
	}

	var journalMode string
	if err := db.QueryRow(`PRAGMA journal_mode`).Scan(&journalMode); err != nil {
		t.Fatalf("PRAGMA journal_mode: %v", err)
	}
	if journalMode != "wal" {
		t.Fatalf("journal_mode = %q, want wal", journalMode)
	}
	var busyTimeout int
	if err := db.QueryRow(`PRAGMA busy_timeout`).Scan(&busyTimeout); err != nil {
		t.Fatalf("PRAGMA busy_timeout: %v", err)
	}
	if busyTimeout != 5000 {
		t.Fatalf("busy_timeout = %d, want 5000", busyTimeout)
	}

	const callers = 16
	var wg sync.WaitGroup
	errCh := make(chan error, callers)
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := EnsureIndexReady(context.Background())
			if err == nil && got != db {
				t.Errorf("EnsureIndexReady returned a second handle")
			}
			errCh <- err
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatalf("concurrent EnsureIndexReady: %v", err)
		}
	}
}

func TestEnsureIndexReadyRebuildsEmbeddedStoreAndUpserts(t *testing.T) {
	base := useIndexBaseDir(t)
	store, err := sessionsqlite.New(filepath.Join(base, "sessions.db"))
	if err != nil {
		t.Fatalf("open source store: %v", err)
	}
	now := time.Now().UTC()
	sess := &session.Session{
		ID:            "session-1",
		WorkspacePath: "/workspace/one",
		Name:          "before",
		Model:         "test-model",
		Metadata:      map[string]any{"turn_status": "done"},
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	if err := store.Save(context.Background(), sess); err != nil {
		t.Fatalf("save source session: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close source store: %v", err)
	}

	db, err := EnsureIndexReady(context.Background())
	if err != nil {
		t.Fatalf("EnsureIndexReady: %v", err)
	}
	entry, err := GetSessionIndex(db, sess.ID)
	if err != nil {
		t.Fatalf("GetSessionIndex: %v", err)
	}
	if entry == nil || entry.Name != "before" || entry.WorkspacePath != sess.WorkspacePath {
		t.Fatalf("unexpected rebuilt entry: %+v", entry)
	}

	store, err = sessionsqlite.New(filepath.Join(base, "sessions.db"))
	if err != nil {
		t.Fatalf("reopen source store: %v", err)
	}
	sess.Name = "after"
	sess.UpdatedAt = now.Add(time.Minute)
	if err := store.Save(context.Background(), sess); err != nil {
		t.Fatalf("update source session: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close updated source store: %v", err)
	}
	if err := RebuildIndex(context.Background(), db); err != nil {
		t.Fatalf("RebuildIndex: %v", err)
	}
	entry, err = GetSessionIndex(db, sess.ID)
	if err != nil {
		t.Fatalf("GetSessionIndex after rebuild: %v", err)
	}
	if entry == nil || entry.Name != "after" {
		t.Fatalf("rebuild did not upsert existing row: %+v", entry)
	}
}

func TestMigrateIndexAddsColumnsToLegacyTable(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatalf("open legacy index: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE sessions (id TEXT PRIMARY KEY)`); err != nil {
		t.Fatalf("create legacy table: %v", err)
	}
	if err := migrateIndex(db); err != nil {
		t.Fatalf("migrateIndex: %v", err)
	}
	cols := columnSet(db, "sessions")
	for _, name := range []string{"workspace_path", "store_path", "name", "model", "turn_status", "message_count", "prompt_tokens", "created_at", "updated_at", "archived_at"} {
		if !cols[name] {
			t.Errorf("migration did not add %q", name)
		}
	}
	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatalf("read user_version: %v", err)
	}
	if version != 1 {
		t.Fatalf("user_version = %d, want 1", version)
	}
}

func TestSessionIndexReadUsesAuthoritativeStorePath(t *testing.T) {
	base := useIndexBaseDir(t)
	sourcePath := filepath.Join(base, "source.db")
	store, err := sessionsqlite.New(sourcePath)
	if err != nil {
		t.Fatalf("open source store: %v", err)
	}
	now := time.Now().UTC()
	sess := &session.Session{
		ID:        "path-routed",
		Name:      "path routed",
		Model:     "test-model",
		Metadata:  map[string]any{"turn_status": "done"},
		CreatedAt: now,
		UpdatedAt: now,
		Messages: []model.Message{
			{Role: model.RoleUser, Content: "original request", OriginTurnID: "turn-user"},
			{Role: model.RoleAssistant, Content: "final result"},
		},
	}
	if err := store.Save(context.Background(), sess); err != nil {
		t.Fatalf("save source session: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close source store: %v", err)
	}

	db, err := OpenIndex()
	if err != nil {
		t.Fatalf("OpenIndex: %v", err)
	}
	WriteSessionIndex(db, sess, sourcePath)
	detail, err := (&sessionIndexImpl{db: db}).Read(context.Background(), sess.ID)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if detail == nil || detail.LastTurn != "final result" || detail.LastUserInput != "original request" || detail.LastTurnID != "turn-user" {
		t.Fatalf("unexpected detail: %+v", detail)
	}
}

func TestTruncateRunesPreservesUTF8(t *testing.T) {
	value := "你好世界"
	if got := truncateRunes(value, 3); got != "你好世..." {
		t.Fatalf("truncateRunes = %q", got)
	}
}

func TestRebuildIndexDeletesOrphansAfterCompleteScan(t *testing.T) {
	base := useIndexBaseDir(t)
	store, err := sessionsqlite.New(filepath.Join(base, "sessions.db"))
	if err != nil {
		t.Fatalf("open source store: %v", err)
	}
	now := time.Now().UTC()
	if err := store.Save(context.Background(), &session.Session{ID: "live", Metadata: map[string]any{}, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("save source session: %v", err)
	}
	_ = store.Close()

	db, err := OpenIndex()
	if err != nil {
		t.Fatalf("OpenIndex: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO sessions (id) VALUES ('orphan')`); err != nil {
		t.Fatalf("insert orphan: %v", err)
	}
	if err := RebuildIndex(context.Background(), db); err != nil {
		t.Fatalf("RebuildIndex: %v", err)
	}
	if got, err := GetSessionIndex(db, "orphan"); err != nil || got != nil {
		t.Fatalf("orphan remains: entry=%+v err=%v", got, err)
	}
	if got, err := GetSessionIndex(db, "live"); err != nil || got == nil {
		t.Fatalf("live row missing: entry=%+v err=%v", got, err)
	}
}

func TestStoreMetadataUpdatesRefreshIndex(t *testing.T) {
	useIndexBaseDir(t)
	store, err := openSQLiteStore("/workspace/ignored-in-embedded-mode")
	if err != nil {
		t.Fatalf("openSQLiteStore: %v", err)
	}
	defer store.Close()
	now := time.Now().UTC()
	sess := &session.Session{ID: "metadata", Name: "before", Metadata: map[string]any{}, CreatedAt: now, UpdatedAt: now}
	if err := store.Save(context.Background(), sess); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := store.UpdateName(context.Background(), sess.ID, "after"); err != nil {
		t.Fatalf("UpdateName: %v", err)
	}
	entry, err := GetSessionIndex(IndexDB(), sess.ID)
	if err != nil || entry == nil || entry.Name != "after" {
		t.Fatalf("rename not projected: entry=%+v err=%v", entry, err)
	}
	archiveStore := store.(session.ConversationArchiveStore)
	if _, err := archiveStore.Archive(context.Background(), sess.ID, now); err != nil {
		t.Fatalf("Archive: %v", err)
	}
	entry, err = GetSessionIndex(IndexDB(), sess.ID)
	if err != nil || entry == nil || entry.ArchivedAt == "" {
		t.Fatalf("archive not projected: entry=%+v err=%v", entry, err)
	}
	if err := archiveStore.Restore(context.Background(), sess.ID); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	entry, err = GetSessionIndex(IndexDB(), sess.ID)
	if err != nil || entry == nil || entry.ArchivedAt != "" {
		t.Fatalf("restore not projected: entry=%+v err=%v", entry, err)
	}
}
