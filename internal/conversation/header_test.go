package conversation

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"code-agent/internal/session"
	"code-agent/internal/session/sqlite"
)

type headerOnlyStore struct {
	session.SessionStore
	headers   session.SessionHeaderStore
	fullLoads int
	saves     int
}

func (s *headerOnlyStore) Load(context.Context, string) (*session.Session, error) {
	s.fullLoads++
	return nil, fmt.Errorf("full Load forbidden")
}
func (s *headerOnlyStore) Save(context.Context, *session.Session) error {
	s.saves++
	return fmt.Errorf("header Save forbidden")
}
func (s *headerOnlyStore) LoadHeader(ctx context.Context, id string) (*session.Session, error) {
	return s.headers.LoadHeader(ctx, id)
}

func TestHeaderReanchorAndRebindNeverLoadOrSaveTranscript(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.New(filepath.Join(t.TempDir(), "sessions.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	base := t.TempDir()
	if err := os.MkdirAll(filepath.Join(base, "MyProj"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, sess := range []*session.Session{
		{ID: "legacy", WorkspacePath: "/old/Container/Documents/MyProj"},
		{ID: "external", WorkspacePath: "/old/external", Workspace: session.WorkspaceRef{Root: session.RootExternal, ExtID: "bookmark"}},
	} {
		if err := store.Save(ctx, sess); err != nil {
			t.Fatal(err)
		}
	}
	guard := &headerOnlyStore{SessionStore: store, headers: store}
	repo := NewSQLiteRepository(guard, 1000, 800, "test", base, nil)
	headers := repo.(session.SessionHeaderStore)
	header, err := headers.LoadHeader(ctx, "legacy")
	if err != nil {
		t.Fatal(err)
	}
	if header.Workspace.Root != session.RootWorkspace || mustEval(t, header.WorkspacePath) != mustEval(t, filepath.Join(base, "MyProj")) {
		t.Fatalf("legacy header=%+v", header)
	}
	if needs, err := repo.NeedsRebind(ctx, "legacy"); err != nil || needs {
		t.Fatalf("legacy needs=%v err=%v", needs, err)
	}
	raw, err := store.LoadHeader(ctx, "legacy")
	if err != nil || raw.Workspace.Root != "" {
		t.Fatalf("read-only migration persisted: %+v err=%v", raw, err)
	}
	if needs, err := repo.NeedsRebind(ctx, "external"); err != nil || !needs {
		t.Fatalf("external needs=%v err=%v", needs, err)
	}
	fresh := t.TempDir()
	if err := repo.Rebind(ctx, "external", fresh); err != nil {
		t.Fatal(err)
	}
	if needs, err := repo.NeedsRebind(ctx, "external"); err != nil || needs {
		t.Fatalf("rebound needs=%v err=%v", needs, err)
	}
	header, err = headers.LoadHeader(ctx, "external")
	if err != nil || mustEval(t, header.WorkspacePath) != mustEval(t, fresh) {
		t.Fatalf("external header=%+v err=%v", header, err)
	}
	if guard.fullLoads != 0 || guard.saves != 0 {
		t.Fatalf("full Load=%d Save=%d", guard.fullLoads, guard.saves)
	}
}

type headerLegacyStore struct {
	session.SessionStore
	loads int
}

func (s *headerLegacyStore) Load(ctx context.Context, id string) (*session.Session, error) {
	s.loads++
	return s.SessionStore.Load(ctx, id)
}

func TestHeaderOldStoreFallback(t *testing.T) {
	ctx := context.Background()
	store := session.NewMemoryStore()
	if err := store.Save(ctx, &session.Session{ID: "legacy", Name: "fallback"}); err != nil {
		t.Fatal(err)
	}
	legacy := &headerLegacyStore{SessionStore: store}
	repo := NewSQLiteRepository(legacy, 1000, 800, "test", "", nil)
	header, err := repo.(session.SessionHeaderStore).LoadHeader(ctx, "legacy")
	if err != nil || header.Name != "fallback" || legacy.loads != 1 {
		t.Fatalf("header=%+v loads=%d err=%v", header, legacy.loads, err)
	}
}
