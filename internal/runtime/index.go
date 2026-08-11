// Package runtime — cross-workspace session index.
//
// index.db is a lightweight global SQLite database that stores session
// metadata (id, workspace, name, model, status, timestamps) for every
// session across all workspaces. It is a read-optimised projection:
// writes are best-effort (failure logs and continues) and the index can
// be fully rebuilt from per-workspace sessions.db files.
//
// This is the foundation for Phase A of the cross-session control plane
// (docs/p13-cross-session-control-plane.md): list_sessions and read_session.

package runtime

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"code-agent/internal/session"
	sessionsqlite "code-agent/internal/session/sqlite"
	"code-agent/internal/tools"
)

var (
	indexMu             sync.Mutex
	indexDB             *sql.DB
	indexDBPath         string
	indexRebuildChecked bool
)

// --- index path ---

// indexPath returns the path to the global session index database.
// It lives beside the per-project stores: $HOME/.codeagent/index.db on
// desktop, or <storeBaseDir>/index.db on iOS/embedded.
func indexPath() (string, error) {
	base := storeBaseDir
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		base = filepath.Join(home, ".codeagent")
	}
	return filepath.Join(base, "index.db"), nil
}

// --- schema ---

const indexTableSchema = `
CREATE TABLE IF NOT EXISTS sessions (
	id              TEXT PRIMARY KEY,
	workspace_path  TEXT NOT NULL DEFAULT '',
	store_path      TEXT NOT NULL DEFAULT '',
	name            TEXT NOT NULL DEFAULT '',
	model           TEXT NOT NULL DEFAULT '',
	turn_status     TEXT NOT NULL DEFAULT '',
	message_count   INTEGER NOT NULL DEFAULT 0,
	prompt_tokens   INTEGER NOT NULL DEFAULT 0,
	created_at      TEXT NOT NULL DEFAULT '',
	updated_at      TEXT NOT NULL DEFAULT '',
	archived_at     TEXT
);
`

const indexIndexes = `
CREATE INDEX IF NOT EXISTS idx_sessions_workspace ON sessions(workspace_path);
CREATE INDEX IF NOT EXISTS idx_sessions_status    ON sessions(turn_status);
CREATE INDEX IF NOT EXISTS idx_sessions_updated   ON sessions(updated_at);
`

// --- open ---

// OpenIndex opens (creating if needed) the process-wide global session index.
// Repeated calls for the same configured base directory return the same handle.
func OpenIndex() (*sql.DB, error) {
	path, err := indexPath()
	if err != nil {
		return nil, fmt.Errorf("index: resolve path: %w", err)
	}

	indexMu.Lock()
	defer indexMu.Unlock()
	if indexDB != nil && indexDBPath == path {
		return indexDB, nil
	}
	if indexDB != nil {
		_ = indexDB.Close()
		indexDB = nil
		indexDBPath = ""
		indexRebuildChecked = false
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("index: create dir: %w", err)
	}
	db, err := sql.Open("sqlite", path+"?mode=rwc&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)")
	if err != nil {
		return nil, fmt.Errorf("index: open: %w", err)
	}
	// Serialize writes within this process. WAL + busy_timeout coordinates with
	// readers and writers in other codeagent processes sharing index.db.
	db.SetMaxOpenConns(1)
	if err := migrateIndex(db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("index: migrate: %w", err)
	}
	indexDB = db
	indexDBPath = path
	return indexDB, nil
}

func migrateIndex(db *sql.DB) error {
	if _, err := db.Exec(indexTableSchema); err != nil {
		return err
	}
	for _, stmt := range []string{
		`ALTER TABLE sessions ADD COLUMN workspace_path TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE sessions ADD COLUMN store_path TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE sessions ADD COLUMN name TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE sessions ADD COLUMN model TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE sessions ADD COLUMN turn_status TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE sessions ADD COLUMN message_count INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE sessions ADD COLUMN prompt_tokens INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE sessions ADD COLUMN created_at TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE sessions ADD COLUMN updated_at TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE sessions ADD COLUMN archived_at TEXT`,
	} {
		if _, err := db.Exec(stmt); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			return fmt.Errorf("%s: %w", stmt, err)
		}
	}
	if _, err := db.Exec(indexIndexes); err != nil {
		return err
	}
	_, err := db.Exec(`PRAGMA user_version=1`)
	return err
}

// EnsureIndexReady opens the global index and, once per process, rebuilds it
// synchronously when it is empty. A partial rebuild still returns the usable DB
// handle together with the error so later Save/Delete hooks remain active.
func EnsureIndexReady(ctx context.Context) (*sql.DB, error) {
	db, err := OpenIndex()
	if err != nil {
		return nil, err
	}

	indexMu.Lock()
	defer indexMu.Unlock()
	if indexRebuildChecked {
		return db, nil
	}

	var count int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sessions`).Scan(&count); err != nil {
		return db, fmt.Errorf("index: count sessions: %w", err)
	}
	if count > 0 {
		indexRebuildChecked = true
		return db, nil
	}
	if err := RebuildIndex(ctx, db); err != nil {
		return db, err
	}
	indexRebuildChecked = true
	fmt.Fprintln(os.Stderr, "[index] ✅ index ready")
	return db, nil
}

func resetIndex() {
	indexMu.Lock()
	defer indexMu.Unlock()
	if indexDB != nil {
		_ = indexDB.Close()
	}
	indexDB = nil
	indexDBPath = ""
	indexRebuildChecked = false
}

// --- write hooks (called from sqlite.Store after primary write succeeds) ---

// IndexWriteHook is the callback signature for index updates. The sqlite
// store calls it after a successful Save or Delete. Implementations must
// be best-effort — a failure here must not fail the primary write.
type IndexWriteHook func(sess *session.Session, storePath string)

// IndexDeleteHook is the callback signature for index deletes.
type IndexDeleteHook func(sessionID string)

// --- write ---

// WriteSessionIndex upserts a row into index.db after a successful
// session save. Best-effort: failure logs to stderr and returns nil.
func WriteSessionIndex(db *sql.DB, sess *session.Session, storePath string) {
	if db == nil {
		return
	}
	turnStatus := ""
	if st, ok := sess.Metadata["turn_status"]; ok {
		if s, ok := st.(string); ok {
			turnStatus = s
		}
	}
	_, err := db.Exec(`
		INSERT INTO sessions (id, workspace_path, store_path, name, model, turn_status, message_count, prompt_tokens, created_at, updated_at, archived_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			workspace_path=excluded.workspace_path,
			store_path=excluded.store_path,
			name=excluded.name,
			model=excluded.model,
			turn_status=excluded.turn_status,
			message_count=excluded.message_count,
			prompt_tokens=excluded.prompt_tokens,
			updated_at=excluded.updated_at,
			archived_at=excluded.archived_at
	`,
		sess.ID,
		sess.WorkspacePath,
		storePath,
		sess.Name,
		sess.Model,
		turnStatus,
		len(sess.Messages),
		sess.PromptTokens,
		sess.CreatedAt.UTC().Format(time.RFC3339),
		sess.UpdatedAt.UTC().Format(time.RFC3339),
		archivedAt(sess.ArchivedAt),
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[index] write session %s: %v\n", sess.ID, err)
	}
}

// DeleteSessionIndex removes a row from index.db after a successful
// session delete. Best-effort: failure logs to stderr and returns nil.
func DeleteSessionIndex(db *sql.DB, sessionID string) {
	if db == nil {
		return
	}
	if _, err := db.Exec(`DELETE FROM sessions WHERE id=?`, sessionID); err != nil {
		fmt.Fprintf(os.Stderr, "[index] delete session %s: %v\n", sessionID, err)
	}
}

func archivedAt(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UTC().Format(time.RFC3339)
}

// --- read ---

// IndexSession is the row returned by ListAllSessions.
type IndexSession struct {
	ID            string `json:"id"`
	WorkspacePath string `json:"workspace_path"`
	StorePath     string `json:"-"`
	Name          string `json:"name"`
	Model         string `json:"model"`
	TurnStatus    string `json:"turn_status"`
	MessageCount  int    `json:"message_count"`
	PromptTokens  int    `json:"prompt_tokens"`
	UpdatedAt     string `json:"updated_at"`
	ArchivedAt    string `json:"archived_at,omitempty"`
}

// ListAllSessions returns every session recorded in the index, newest
// first. If db is nil, returns an empty slice.
func ListAllSessions(db *sql.DB) ([]IndexSession, error) {
	if db == nil {
		return nil, nil
	}
	rows, err := db.Query(`SELECT id, workspace_path, name, model, turn_status, message_count, prompt_tokens, updated_at, COALESCE(archived_at, '') FROM sessions ORDER BY updated_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []IndexSession
	for rows.Next() {
		var s IndexSession
		var updated sql.NullString
		if err := rows.Scan(&s.ID, &s.WorkspacePath, &s.Name, &s.Model, &s.TurnStatus, &s.MessageCount, &s.PromptTokens, &updated, &s.ArchivedAt); err != nil {
			return out, err
		}
		if updated.Valid {
			s.UpdatedAt = updated.String
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// GetSessionIndex returns the index row for a single session, or nil if
// not found.
func GetSessionIndex(db *sql.DB, id string) (*IndexSession, error) {
	if db == nil {
		return nil, nil
	}
	var s IndexSession
	var updated sql.NullString
	err := db.QueryRow(
		`SELECT id, workspace_path, store_path, name, model, turn_status, message_count, prompt_tokens, updated_at, COALESCE(archived_at, '') FROM sessions WHERE id=?`,
		id,
	).Scan(&s.ID, &s.WorkspacePath, &s.StorePath, &s.Name, &s.Model, &s.TurnStatus, &s.MessageCount, &s.PromptTokens, &updated, &s.ArchivedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if updated.Valid {
		s.UpdatedAt = updated.String
	}
	return &s, nil
}

// IndexDB returns the package-level global index database handle, or nil if
// OpenIndex has not been called or failed. Tools and HTTP handlers use this
// to query the cross-workspace session list.
func IndexDB() *sql.DB { return indexDB }

// SessionIndex returns a tools.SessionIndex backed by the global index.
// Returns nil if the index is unavailable.
func SessionIndex() tools.SessionIndex {
	if indexDB == nil {
		return nil
	}
	return &sessionIndexImpl{db: indexDB}
}

// sessionIndexImpl implements tools.SessionIndex.
type sessionIndexImpl struct{ db *sql.DB }

func (s *sessionIndexImpl) ListAll() ([]tools.SessionIndexEntry, error) {
	rows, err := ListAllSessions(s.db)
	if err != nil {
		return nil, err
	}
	out := make([]tools.SessionIndexEntry, len(rows))
	for i, r := range rows {
		out[i] = tools.SessionIndexEntry{
			ID:            r.ID,
			WorkspacePath: r.WorkspacePath,
			Name:          r.Name,
			Model:         r.Model,
			TurnStatus:    r.TurnStatus,
			MessageCount:  r.MessageCount,
			PromptTokens:  r.PromptTokens,
			UpdatedAt:     r.UpdatedAt,
			ArchivedAt:    r.ArchivedAt,
		}
	}
	return out, nil
}

func (s *sessionIndexImpl) Get(id string) (*tools.SessionIndexEntry, error) {
	r, err := GetSessionIndex(s.db, id)
	if err != nil || r == nil {
		return nil, err
	}
	return &tools.SessionIndexEntry{
		ID:            r.ID,
		WorkspacePath: r.WorkspacePath,
		Name:          r.Name,
		Model:         r.Model,
		TurnStatus:    r.TurnStatus,
		MessageCount:  r.MessageCount,
		PromptTokens:  r.PromptTokens,
		UpdatedAt:     r.UpdatedAt,
		ArchivedAt:    r.ArchivedAt,
	}, nil
}

func (s *sessionIndexImpl) Read(ctx context.Context, id string) (*tools.SessionIndexDetail, error) {
	entry, err := GetSessionIndex(s.db, id)
	if err != nil {
		return nil, err
	}
	if entry == nil {
		return nil, nil
	}
	// store_path is the routing authority. Legacy sessions can have an empty
	// workspace_path, while their indexed database path remains exact.
	var store session.Store
	if entry.StorePath != "" {
		store, err = sessionsqlite.NewReadOnly(entry.StorePath)
	} else {
		store, err = OpenStore(entry.WorkspacePath)
	}
	if err != nil {
		return nil, fmt.Errorf("index: read session %s: open workspace store: %w", id, err)
	}
	defer store.Close()
	sess, err := store.Load(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("index: read session %s: load: %w", id, err)
	}
	lastTurn := ""
	lastTurnID := ""
	lastUserInput := ""
	for i := len(sess.Messages) - 1; i >= 0; i-- {
		message := sess.Messages[i]
		if lastUserInput == "" && message.Role == "user" && message.Content != "" {
			lastUserInput = message.Content
			lastTurnID = message.OriginTurnID
		}
		if lastTurn == "" && message.Role == "assistant" && message.Content != "" {
			lastTurn = message.Content
		}
		if lastTurn != "" && lastUserInput != "" {
			break
		}
	}
	lastTurn = truncateRunes(lastTurn, 2000)
	lastUserInput = truncateRunes(lastUserInput, 2000)
	ts := ""
	if st, ok := sess.Metadata["turn_status"]; ok {
		if s, ok := st.(string); ok {
			ts = s
		}
	}
	return &tools.SessionIndexDetail{
		ID:            sess.ID,
		WorkspacePath: sess.WorkspacePath,
		Name:          sess.Name,
		Model:         sess.Model,
		TurnStatus:    ts,
		MessageCount:  len(sess.Messages),
		PromptTokens:  sess.PromptTokens,
		Summary:       sess.Summary,
		LastTurn:      lastTurn,
		LastTurnID:    lastTurnID,
		LastUserInput: lastUserInput,
		CreatedAt:     sess.CreatedAt.UTC().Format(time.RFC3339),
		UpdatedAt:     sess.UpdatedAt.UTC().Format(time.RFC3339),
	}, nil
}

// Search runs a keyword search across every session in the index, opening
// each workspace's store read-only once and merging the LIKE hits. Store path
// is the routing authority (see Read); legacy rows fall back to the workspace
// root. Name matches rank above summary matches above message-content matches;
// recency breaks ties.
func (s *sessionIndexImpl) Search(ctx context.Context, query string, limit int) ([]tools.SessionSearchResult, error) {
	if s.db == nil {
		return nil, nil
	}
	if limit <= 0 {
		limit = 10
	}
	fetch := limit * 4 // over-fetch so per-store LIMITs don't starve distinct sessions
	if fetch < 50 {
		fetch = 50
	}

	entries, err := s.ListAll()
	if err != nil {
		return nil, err
	}
	// store_path is the routing authority (see Read); fetch it directly so
	// ListAllSessions' projection stays unchanged.
	pathRows, err := s.db.Query(`SELECT id, workspace_path, COALESCE(store_path, '') FROM sessions`)
	if err != nil {
		return nil, err
	}
	routes := map[string][2]string{} // id -> {store_path, workspace_path}
	for pathRows.Next() {
		var id, workspace, store string
		if err := pathRows.Scan(&id, &workspace, &store); err != nil {
			pathRows.Close()
			return nil, err
		}
		routes[id] = [2]string{store, workspace}
	}
	pathRows.Close()
	if err := pathRows.Err(); err != nil {
		return nil, err
	}

	type group struct {
		store   session.Store
		entries []tools.SessionIndexEntry
	}
	// SearchMessages is the optional per-store search capability (sqlite.Store).
	// Stores that do not implement it simply contribute no hits.
	type messageSearcher interface {
		SearchMessages(ctx context.Context, query string, limit int) ([]session.MessageHit, error)
	}
	groups := map[string]*group{}
	var order []string
	for _, e := range entries {
		if e.ArchivedAt != "" {
			continue
		}
		r := routes[e.ID]
		key := r[0] // store_path
		if key == "" {
			key = r[1] // workspace_path fallback
		}
		if key == "" {
			continue
		}
		g := groups[key]
		if g == nil {
			var st session.Store
			if r[0] != "" {
				st, err = sessionsqlite.NewReadOnly(r[0])
			} else {
				st, err = OpenStore(r[1])
			}
			if err != nil {
				continue // store gone (workspace deleted); skip its sessions
			}
			g = &group{store: st}
			groups[key] = g
			order = append(order, key)
		}
		g.entries = append(g.entries, e)
	}
	defer func() {
		for _, key := range order {
			_ = groups[key].store.Close()
		}
	}()

	type merged struct {
		entry   tools.SessionIndexEntry
		best    int // 0 name, 1 summary, 2 content
		snippet string
		role    string
	}
	byID := map[string]*merged{}
	for _, key := range order {
		g := groups[key]
		searcher, ok := g.store.(messageSearcher)
		if !ok {
			continue
		}
		hits, err := searcher.SearchMessages(ctx, query, fetch)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[search_session] store %s: %v\n", key, err)
			continue
		}
		for _, h := range hits {
			m := byID[h.SessionID]
			if m == nil {
				idx := -1
				for i := range g.entries {
					if g.entries[i].ID == h.SessionID {
						idx = i
						break
					}
				}
				if idx < 0 {
					continue // hit for a session not in the index
				}
				m = &merged{entry: g.entries[idx]}
				byID[h.SessionID] = m
			}
			rank := 2
			if h.Kind == "name" {
				rank = 0
			} else if h.Kind == "summary" {
				rank = 1
			}
			if rank < m.best || m.snippet == "" {
				m.best = rank
				m.snippet = h.Snippet
				m.role = h.Role
			}
		}
	}

	results := make([]tools.SessionSearchResult, 0, len(byID))
	for _, m := range byID {
		results = append(results, tools.SessionSearchResult{
			ID:            m.entry.ID,
			WorkspacePath: m.entry.WorkspacePath,
			Name:          m.entry.Name,
			Model:         m.entry.Model,
			TurnStatus:    m.entry.TurnStatus,
			MessageCount:  m.entry.MessageCount,
			UpdatedAt:     m.entry.UpdatedAt,
			Snippet:       truncateRunes(m.snippet, 400),
			Role:          m.role,
			Score:         m.best,
		})
	}
	sort.SliceStable(results, func(i, j int) bool {
		if results[i].Score != results[j].Score {
			return results[i].Score < results[j].Score
		}
		return results[i].UpdatedAt > results[j].UpdatedAt // recency tiebreak
	})
	if len(results) > limit {
		results = results[:limit]
	}
	return results, nil
}

func truncateRunes(value string, limit int) string {
	if limit < 0 {
		limit = 0
	}
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit]) + "..."
}

// --- rebuild ---

// RebuildIndex scans all desktop per-workspace stores, or the single embedded
// store, and upserts their session metadata into the index.
func RebuildIndex(ctx context.Context, db *sql.DB) error {
	if db == nil {
		return nil
	}
	base := storeBaseDir
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		base = filepath.Join(home, ".codeagent")
	}
	var storePaths []string
	if storeBaseDir != "" {
		storePath := filepath.Join(base, "sessions.db")
		if _, err := os.Stat(storePath); err == nil {
			storePaths = append(storePaths, storePath)
		} else if !os.IsNotExist(err) {
			return err
		}
	} else {
		projectsDir := filepath.Join(base, "projects")
		entries, err := os.ReadDir(projectsDir)
		if err != nil {
			if os.IsNotExist(err) {
				return nil // no projects yet — empty index is correct
			}
			return err
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			storePath := filepath.Join(projectsDir, entry.Name(), "sessions.db")
			if _, err := os.Stat(storePath); err == nil {
				storePaths = append(storePaths, storePath)
			} else if !os.IsNotExist(err) {
				return err
			}
		}
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("index: begin rebuild: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `CREATE TEMP TABLE IF NOT EXISTS rebuild_seen_sessions (id TEXT PRIMARY KEY)`); err != nil {
		return fmt.Errorf("index: create rebuild seen set: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM rebuild_seen_sessions`); err != nil {
		return fmt.Errorf("index: reset rebuild seen set: %w", err)
	}

	var rebuildErrs []error
	for _, storePath := range storePaths {
		if err := rebuildFromStore(ctx, tx, storePath); err != nil {
			fmt.Fprintf(os.Stderr, "[index] rebuild: %s: %v\n", storePath, err)
			rebuildErrs = append(rebuildErrs, fmt.Errorf("%s: %w", storePath, err))
		}
	}
	if len(rebuildErrs) == 0 {
		if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE id NOT IN (SELECT id FROM rebuild_seen_sessions)`); err != nil {
			return fmt.Errorf("index: delete orphaned rows: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("index: commit rebuild: %w", err)
	}
	return errors.Join(rebuildErrs...)
}

type indexExecer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func rebuildFromStore(ctx context.Context, dst indexExecer, storePath string) error {
	storeDB, err := sql.Open("sqlite", storePath+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		return err
	}
	defer storeDB.Close()

	// Detect which optional columns exist in this store's schema. Older
	// session databases may lack columns added in later versions.
	cols := columnSet(storeDB, "sessions")

	// Try the full query first; fall back to a minimal query for stores
	// that predate workspace_path / name / archived_at.
	rows, err := storeDB.QueryContext(ctx, rebuildQuery(cols))
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id, model, createdAt, updatedAt, turnStatus string
		var wsp, name sql.NullString
		var archivedAt sql.NullString
		var promptTokens, messageCount int
		scanArgs := []any{&id, &model, &promptTokens, &createdAt, &updatedAt, &messageCount, &turnStatus}
		if cols["workspace_path"] {
			scanArgs = append(scanArgs, &wsp)
		}
		if cols["name"] {
			scanArgs = append(scanArgs, &name)
		}
		if cols["archived_at"] {
			scanArgs = append(scanArgs, &archivedAt)
		}
		if err := rows.Scan(scanArgs...); err != nil {
			return fmt.Errorf("scan: %w", err)
		}
		workspacePath := ""
		if wsp.Valid {
			workspacePath = wsp.String
		}
		sessionName := ""
		if name.Valid {
			sessionName = name.String
		}
		_, err := dst.ExecContext(ctx, `
			INSERT INTO sessions (id, workspace_path, store_path, name, model, turn_status, message_count, prompt_tokens, created_at, updated_at, archived_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(id) DO UPDATE SET
				workspace_path=excluded.workspace_path,
				store_path=excluded.store_path,
				name=excluded.name,
				model=excluded.model,
				turn_status=excluded.turn_status,
				message_count=excluded.message_count,
				prompt_tokens=excluded.prompt_tokens,
				created_at=excluded.created_at,
				updated_at=excluded.updated_at,
				archived_at=excluded.archived_at
		`, id, workspacePath, storePath, sessionName, model, turnStatus, messageCount, promptTokens, createdAt, updatedAt, archivedAtStr(archivedAt))
		if err != nil {
			return fmt.Errorf("write row %s: %w", id, err)
		}
		if _, err := dst.ExecContext(ctx, `INSERT OR IGNORE INTO rebuild_seen_sessions(id) VALUES (?)`, id); err != nil {
			return fmt.Errorf("record rebuilt row %s: %w", id, err)
		}
	}
	return rows.Err()
}

// columnSet returns the set of column names present in a table.
func columnSet(db *sql.DB, table string) map[string]bool {
	rows, err := db.Query(fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return nil
	}
	defer rows.Close()
	set := make(map[string]bool)
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull int
		var dflt sql.NullString
		var pk int
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			continue
		}
		set[name] = true
	}
	return set
}

// rebuildQuery builds a SELECT that works with the given column set, so
// stores predating later schema additions still rebuild successfully.
func rebuildQuery(cols map[string]bool) string {
	var extra []string
	if cols["workspace_path"] {
		extra = append(extra, "COALESCE(s.workspace_path, '') as workspace_path")
	}
	if cols["name"] {
		extra = append(extra, "COALESCE(s.name, '') as name")
	}
	if cols["archived_at"] {
		extra = append(extra, "s.archived_at")
	}
	// metadata column may be absent (very early schemas) or contain NULL /
	// non-JSON values. Use a CASE guard to avoid json_extract errors.
	turnStatus := "'' as turn_status"
	if cols["metadata"] {
		turnStatus = "CASE WHEN s.metadata IS NULL OR s.metadata = '' OR json_valid(s.metadata) = 0 THEN '' ELSE COALESCE(json_extract(s.metadata, '$.turn_status'), '') END as turn_status"
	}
	q := "SELECT s.id, s.model, s.prompt_tokens, s.created_at, s.updated_at, " +
		"COALESCE(m.cnt, 0) as message_count, " +
		turnStatus
	if len(extra) > 0 {
		q += ", " + strings.Join(extra, ", ")
	}
	q += " FROM sessions s LEFT JOIN (SELECT session_id, COUNT(*) as cnt FROM messages GROUP BY session_id) m ON m.session_id = s.id"
	return q
}

func archivedAtStr(s sql.NullString) any {
	if !s.Valid || s.String == "" {
		return nil
	}
	return s.String
}
