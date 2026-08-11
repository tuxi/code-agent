package runtime

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"code-agent/internal/model"
	"code-agent/internal/session"
	sessionsqlite "code-agent/internal/session/sqlite"
)

func TestSessionIndexSearchAcrossStores(t *testing.T) {
	useIndexBaseDir(t)
	idx, err := OpenIndex()
	if err != nil {
		t.Fatalf("OpenIndex: %v", err)
	}
	ctx := context.Background()
	now := time.Now().UTC()
	base := t.TempDir()

	// Store A: name match for 企业 (ranks above everything).
	pathA := filepath.Join(base, "a.db")
	sessA := &session.Session{
		ID: "sess-a", Name: "企业级Agent项目", Metadata: map[string]any{},
		CreatedAt: now, UpdatedAt: now,
		Messages: []model.Message{{Role: model.RoleUser, Content: "无关正文"}},
	}
	stA, err := sessionsqlite.New(pathA)
	if err != nil {
		t.Fatalf("open a: %v", err)
	}
	if err := stA.Save(ctx, sessA); err != nil {
		t.Fatalf("save a: %v", err)
	}
	if err := stA.Close(); err != nil {
		t.Fatalf("close a: %v", err)
	}
	WriteSessionIndex(idx, sessA, pathA)

	// Store B: content match in another workspace.
	pathB := filepath.Join(base, "b.db")
	sessB := &session.Session{
		ID: "sess-b", Name: "", Metadata: map[string]any{},
		CreatedAt: now, UpdatedAt: now,
		Messages: []model.Message{{Role: model.RoleAssistant, Content: "企业级网关超时修复方案"}},
	}
	stB, err := sessionsqlite.New(pathB)
	if err != nil {
		t.Fatalf("open b: %v", err)
	}
	if err := stB.Save(ctx, sessB); err != nil {
		t.Fatalf("save b: %v", err)
	}
	if err := stB.Close(); err != nil {
		t.Fatalf("close b: %v", err)
	}
	WriteSessionIndex(idx, sessB, pathB)

	// Store C: archived session must be excluded.
	pathC := filepath.Join(base, "c.db")
	sessC := &session.Session{
		ID: "sess-c", Name: "", Metadata: map[string]any{},
		CreatedAt: now, UpdatedAt: now, ArchivedAt: now,
		Messages: []model.Message{{Role: model.RoleUser, Content: "企业级归档内容"}},
	}
	stC, err := sessionsqlite.New(pathC)
	if err != nil {
		t.Fatalf("open c: %v", err)
	}
	if err := stC.Save(ctx, sessC); err != nil {
		t.Fatalf("save c: %v", err)
	}
	if err := stC.Close(); err != nil {
		t.Fatalf("close c: %v", err)
	}
	WriteSessionIndex(idx, sessC, pathC)

	impl := &sessionIndexImpl{db: idx}
	res, err := impl.Search(ctx, "企业", 10)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(res) != 2 {
		t.Fatalf("expected 2 results (archived excluded), got %d: %+v", len(res), res)
	}
	if res[0].ID != "sess-a" {
		t.Fatalf("name match should rank first, got %+v", res[0])
	}
	if res[0].Snippet != "企业级Agent项目" {
		t.Fatalf("name snippet mismatch: %q", res[0].Snippet)
	}
	if res[1].ID != "sess-b" {
		t.Fatalf("expected sess-b second, got %+v", res[1])
	}
	if res[1].Role != "assistant" {
		t.Fatalf("content hit should carry role, got %q", res[1].Role)
	}
}

func TestSessionIndexSearchDedupesAndLimits(t *testing.T) {
	useIndexBaseDir(t)
	idx, err := OpenIndex()
	if err != nil {
		t.Fatalf("OpenIndex: %v", err)
	}
	ctx := context.Background()
	now := time.Now().UTC()
	base := t.TempDir()

	// One store, three sessions; every session matches "企业" so dedupe keeps
	// one entry per session and the limit truncates.
	path := filepath.Join(base, "single.db")
	st, err := sessionsqlite.New(path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	for i := 0; i < 3; i++ {
		sess := &session.Session{
			ID: "sess-" + string(rune('a'+i)), Metadata: map[string]any{},
			CreatedAt: now, UpdatedAt: now,
			Messages: []model.Message{
				{Role: model.RoleUser, Content: "企业级项目讨论"},
				{Role: model.RoleAssistant, Content: "企业级网关方案"},
			},
		}
		if err := st.Save(ctx, sess); err != nil {
			t.Fatalf("save %d: %v", i, err)
		}
		WriteSessionIndex(idx, sess, path)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	impl := &sessionIndexImpl{db: idx}
	res, err := impl.Search(ctx, "企业", 2)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(res) != 2 {
		t.Fatalf("limit=2 should truncate to 2 results, got %d", len(res))
	}
	seen := map[string]bool{}
	for _, r := range res {
		if seen[r.ID] {
			t.Fatalf("duplicate session %s in results", r.ID)
		}
		seen[r.ID] = true
	}
}

func TestSessionIndexSearchEmptyQuery(t *testing.T) {
	useIndexBaseDir(t)
	idx, err := OpenIndex()
	if err != nil {
		t.Fatalf("OpenIndex: %v", err)
	}
	impl := &sessionIndexImpl{db: idx}
	res, err := impl.Search(context.Background(), "", 10)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(res) != 0 {
		t.Fatalf("blank query should return nothing, got %d", len(res))
	}
}
