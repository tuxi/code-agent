package sqlite

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"code-agent/internal/model"
	"code-agent/internal/session"
	_ "modernc.org/sqlite"
)

func saveSearchStore(t *testing.T, sess *session.Session) *Store {
	t.Helper()
	st, err := New(filepath.Join(t.TempDir(), "sessions.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := st.Save(context.Background(), sess); err != nil {
		t.Fatalf("save session: %v", err)
	}
	return st
}

func searchSession(t *testing.T, id, name, summary string, messages []model.Message) *session.Session {
	t.Helper()
	now := time.Now().UTC()
	return &session.Session{
		ID:        id,
		Name:      name,
		Summary:   summary,
		Metadata:  map[string]any{},
		CreatedAt: now,
		UpdatedAt: now,
		Messages:  messages,
	}
}

func TestSearchMessagesContentHit(t *testing.T) {
	sess := searchSession(t, "s1", "", "", []model.Message{
		{Role: model.RoleUser, Content: "修复企业级网关的超时问题"},
		{Role: model.RoleAssistant, Content: "把超时从 5s 提到 30s"},
	})
	st := saveSearchStore(t, sess)
	defer st.Close()

	hits, err := st.SearchMessages(context.Background(), "企业", 10)
	if err != nil {
		t.Fatalf("SearchMessages: %v", err)
	}
	if len(hits) == 0 {
		t.Fatal("expected content hits for 企业")
	}
	if hits[0].SessionID != "s1" || hits[0].Kind != "content" {
		t.Fatalf("unexpected hit: %+v", hits[0])
	}
	if hits[0].Snippet == "" {
		t.Fatal("snippet should not be empty")
	}
}

func TestSearchMessagesNameRanksAboveContent(t *testing.T) {
	sess := searchSession(t, "s1", "企业级Agent项目", "some summary", []model.Message{
		{Role: model.RoleUser, Content: "这里的正文没有企业两个字"},
	})
	st := saveSearchStore(t, sess)
	defer st.Close()

	hits, err := st.SearchMessages(context.Background(), "企业", 10)
	if err != nil {
		t.Fatalf("SearchMessages: %v", err)
	}
	if len(hits) == 0 {
		t.Fatal("expected hit")
	}
	// Name match must come first (rank 0) and carry the name as snippet.
	if hits[0].Kind != "name" || hits[0].Role != "" || hits[0].Snippet != "企业级Agent项目" {
		t.Fatalf("expected name match first, got %+v", hits[0])
	}
}

func TestSearchMessagesSummaryHit(t *testing.T) {
	sess := searchSession(t, "s1", "", "企业级网关方案，讨论迁移到新架构", nil)
	st := saveSearchStore(t, sess)
	defer st.Close()

	hits, err := st.SearchMessages(context.Background(), "企业级网关", 10)
	if err != nil {
		t.Fatalf("SearchMessages: %v", err)
	}
	if len(hits) == 0 {
		t.Fatal("expected summary hit")
	}
	if hits[0].Kind != "summary" {
		t.Fatalf("expected summary match, got %+v", hits[0])
	}
}

func TestSearchMessagesRequiresAllTerms(t *testing.T) {
	sess := searchSession(t, "s1", "", "", []model.Message{
		{Role: model.RoleUser, Content: "企业级项目落地"},
		{Role: model.RoleUser, Content: "网关"},
	})
	st := saveSearchStore(t, sess)
	defer st.Close()

	// "企业 网关" requires both terms in the same row; the rows split them.
	hits, err := st.SearchMessages(context.Background(), "企业 网关", 10)
	if err != nil {
		t.Fatalf("SearchMessages: %v", err)
	}
	if len(hits) != 0 {
		t.Fatalf("expected no hits for split terms, got %d", len(hits))
	}

	// A single row containing both matches.
	sess2 := searchSession(t, "s2", "", "", []model.Message{
		{Role: model.RoleUser, Content: "企业级网关项目"},
	})
	st2 := saveSearchStore(t, sess2)
	defer st2.Close()
	hits2, err := st2.SearchMessages(context.Background(), "企业 网关", 10)
	if err != nil {
		t.Fatalf("SearchMessages: %v", err)
	}
	if len(hits2) == 0 {
		t.Fatal("expected hit for co-occurring terms")
	}
}

func TestSearchMessagesEscapesLikeMetacharacters(t *testing.T) {
	sess := searchSession(t, "s1", "", "", []model.Message{
		{Role: model.RoleUser, Content: "覆盖率 100% 达成"},
	})
	st := saveSearchStore(t, sess)
	defer st.Close()

	hits, err := st.SearchMessages(context.Background(), "100%", 10)
	if err != nil {
		t.Fatalf("SearchMessages: %v", err)
	}
	if len(hits) == 0 {
		t.Fatal("expected hit for literal % term")
	}
	// "%达" as a literal would match nothing if the % were treated as wildcard
	// anchored mid-string; ensure the percent is escaped properly.
	hits2, err := st.SearchMessages(context.Background(), "%达", 10)
	if err != nil {
		t.Fatalf("SearchMessages: %v", err)
	}
	if len(hits2) != 0 {
		t.Fatalf("expected no hit for wildcard-interpreted %%%%达, got %d", len(hits2))
	}
}

func TestSearchMessagesEmptyQueryOrLimit(t *testing.T) {
	st := saveSearchStore(t, searchSession(t, "s1", "企业", "", nil))
	defer st.Close()

	hits, err := st.SearchMessages(context.Background(), "   ", 10)
	if err != nil || hits != nil {
		t.Fatalf("blank query: hits=%v err=%v, want nil,nil", hits, err)
	}
	hits, err = st.SearchMessages(context.Background(), "企业", 0)
	if err != nil || hits != nil {
		t.Fatalf("zero limit: hits=%v err=%v, want nil,nil", hits, err)
	}
}

// TestSearchMessagesLegacySchemaNoNameColumn simulates a pre-name-column
// database: SearchMessages must skip the name branch instead of failing the
// whole query (which used to zero out every store's hits).
func TestSearchMessagesLegacySchemaNoNameColumn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open legacy db: %v", err)
	}
	if _, err := db.Exec(`
		CREATE TABLE sessions (
			id TEXT PRIMARY KEY, model TEXT, summary TEXT,
			prompt_tokens INTEGER, context_window INTEGER, compact_threshold INTEGER,
			workspace_path TEXT, created_at TEXT, updated_at TEXT, metadata TEXT
		);
		CREATE TABLE messages (
			session_id TEXT, seq INTEGER, role TEXT, content TEXT,
			tool_calls TEXT, tool_call_id TEXT, PRIMARY KEY (session_id, seq)
		);
		INSERT INTO sessions (id, model, summary, created_at, updated_at) VALUES ('legacy1', 'm', '企业级网关方案', '2026-01-01', '2026-01-01');
		INSERT INTO messages (session_id, seq, role, content) VALUES ('legacy1', 0, 'user', '企业级Agent项目落地讨论');
	`); err != nil {
		t.Fatalf("create legacy schema: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close legacy db: %v", err)
	}

	st, err := NewReadOnly(path)
	if err != nil {
		t.Fatalf("open read-only: %v", err)
	}
	defer st.Close()

	hits, err := st.SearchMessages(context.Background(), "企业", 10)
	if err != nil {
		t.Fatalf("SearchMessages on legacy schema: %v", err)
	}
	if len(hits) == 0 {
		t.Fatal("expected content/summary hits on legacy schema")
	}
	for _, h := range hits {
		if h.Kind == "name" {
			t.Fatalf("legacy schema has no name column; got name hit %+v", h)
		}
	}
}
