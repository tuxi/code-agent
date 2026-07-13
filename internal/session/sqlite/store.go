package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"code-agent/internal/model"
	"code-agent/internal/session"

	_ "modernc.org/sqlite" // pure-Go driver, registered as "sqlite" (no cgo)
)

// Store persists sessions in a single SQLite database. Schema is normalized into
// sessions / messages / compactions so the trace is queryable, but Save replaces
// a session's rows wholesale (a snapshot at the turn boundary) — cheap at CLI
// sizes and trivially consistent, with no delta bookkeeping to drift.
//
// Compile-time checks: Store satisfies all storage interfaces.
var (
	_ session.Store               = (*Store)(nil)
	_ session.SessionStore        = (*Store)(nil)
	_ session.EventStore          = (*Store)(nil)
	_ session.EventAttentionStore = (*Store)(nil)
	_ session.TelemetryStore      = (*Store)(nil)
)

type Store struct {
	db   *sql.DB
	path string
}

// New opens (creating if needed) a SQLite session database at the given path.
func New(path string) (*Store, error) {
	s := &Store{path: path}
	if err := s.open(); err != nil {
		return nil, err
	}
	return s, nil
}

const schema = `
CREATE TABLE IF NOT EXISTS sessions (
	id                TEXT PRIMARY KEY,
	model             TEXT,
	summary           TEXT,
	prompt_tokens     INTEGER,
	context_window    INTEGER,
	compact_threshold INTEGER,
	workspace_path    TEXT,
	workspace_root    TEXT,
	workspace_rel     TEXT,
	workspace_ext_id  TEXT,
	name              TEXT,
	created_at        TEXT,
	updated_at        TEXT,
	metadata          TEXT
	,gateway_assets   TEXT
	,reference_ledger TEXT
);
CREATE TABLE IF NOT EXISTS messages (
	session_id   TEXT,
	seq          INTEGER,
	role         TEXT,
	content      TEXT,
	tool_calls   TEXT,
	tool_call_id TEXT,
	assets       TEXT,
	PRIMARY KEY (session_id, seq)
);
CREATE TABLE IF NOT EXISTS compactions (
	session_id        TEXT,
	seq               INTEGER,
	before_tokens     INTEGER,
	after_tokens      INTEGER,
	saved_tokens      INTEGER,
	compression_ratio REAL,
	summary_chars     INTEGER,
	compacted_at      TEXT,
	PRIMARY KEY (session_id, seq)
);
CREATE TABLE IF NOT EXISTS requests (
	id                INTEGER PRIMARY KEY AUTOINCREMENT,
	at                TEXT,
	model             TEXT,
	prompt_tokens     INTEGER,
	completion_tokens INTEGER,
	attempts          INTEGER,
	retries           INTEGER,
	timed_out         INTEGER,
	success           INTEGER,
	error_class       TEXT,
	latency_ms        INTEGER,
	trace             TEXT
);
CREATE TABLE IF NOT EXISTS session_events (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	session_id TEXT,
	turn_id    TEXT,
	kind       TEXT,
	at         TEXT,
	payload    TEXT
);
CREATE INDEX IF NOT EXISTS idx_session_events_session ON session_events(session_id, at);`

// open (re)opens the database at s.path and applies the idempotent migrations.
// Used at construction and to recover the connection after the file moved out
// from under it (SQLITE_READONLY_DBMOVED) — see Save.
func (s *Store) open() error {
	// WAL + synchronous=NORMAL is the crash-safe storage config the iOS lifecycle
	// contract requires (docs/protocols/agent-wire-v1.2-lifecycle-suspend-resume.md
	// §2.2.1): a per-turn-iteration checkpoint must survive a jetsam SIGKILL
	// mid-write as "last committed boundary or nothing, never half". In WAL mode
	// synchronous=NORMAL is durable against app/process kill (only host power-loss
	// can lose the last committed txn — not our failure mode) and avoids an fsync
	// per commit, which matters now that we checkpoint every loop iteration, not
	// just at the turn boundary. busy_timeout lets a write wait out a concurrent
	// reader instead of failing "database is locked".
	//
	// Pragmas ride in the DSN so the driver applies them on EVERY connection it
	// opens, not once: journal_mode persists in the file header, but synchronous
	// and busy_timeout are per-connection and would be lost if the pool reconnected.
	// modernc strips this query from the file path for a non-"file:" DSN.
	dsn := s.path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return fmt.Errorf("open sqlite %q: %w", s.path, err)
	}
	// Serial CLI usage: a single connection sidesteps SQLite "database is locked".
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return fmt.Errorf("migrate: %w", err)
	}
	// Additive migrations: columns added after the requests table first shipped
	// won't come from CREATE IF NOT EXISTS. ADD COLUMN is idempotent here —
	// "duplicate column" just means it already applied.
	for _, stmt := range []string{
		`ALTER TABLE requests ADD COLUMN trace TEXT`,
		`ALTER TABLE requests ADD COLUMN completion_tokens INTEGER`,
		`ALTER TABLE requests ADD COLUMN cached_prompt_tokens INTEGER`,
		`ALTER TABLE sessions ADD COLUMN metadata TEXT`,
		`ALTER TABLE sessions ADD COLUMN workspace_path TEXT`,
		// Portable workspace identity (iOS reinstall safety): persisted instead of the
		// frozen absolute workspace_path, which now serves only as a display hint.
		`ALTER TABLE sessions ADD COLUMN workspace_root TEXT`,
		`ALTER TABLE sessions ADD COLUMN workspace_rel TEXT`,
		`ALTER TABLE sessions ADD COLUMN workspace_ext_id TEXT`,
		`ALTER TABLE sessions ADD COLUMN name TEXT`,
		`ALTER TABLE sessions ADD COLUMN gateway_assets TEXT`,
		`ALTER TABLE sessions ADD COLUMN reference_ledger TEXT`,
		`ALTER TABLE messages ADD COLUMN assets TEXT`,
		// v2: re-index session_events by at for chronological ordering.
		// The original index was on (session_id, id); rebuild on (session_id, at).
		`DROP INDEX IF EXISTS idx_session_events_session`,
		`CREATE INDEX idx_session_events_session ON session_events(session_id, at)`,
	} {
		if _, err := db.Exec(stmt); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			db.Close()
			return fmt.Errorf("migrate (%s): %w", stmt, err)
		}
	}
	s.db = db
	return nil
}

// reopen closes the (possibly poisoned) connection and opens a fresh one at the
// same path. After SQLITE_READONLY_DBMOVED the old connection is stuck; only a
// reopen — which binds to the file currently at the path — can write again.
func (s *Store) reopen() error {
	if s.db != nil {
		_ = s.db.Close()
	}
	return s.open()
}

func (s *Store) Close() error { return s.db.Close() }

// Save persists a session as a wholesale snapshot. If the write fails because
// the database file moved out from under the open connection
// (SQLITE_READONLY_DBMOVED — classically a synced folder like iCloud replacing
// the file), it reopens and retries once.
func (s *Store) Save(ctx context.Context, sess *session.Session) error {
	err := s.save(ctx, sess)
	if err != nil && isReadonlyErr(err) {
		if rerr := s.reopen(); rerr == nil {
			err = s.save(ctx, sess)
		}
	}
	return err
}

func isReadonlyErr(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "readonly")
}

func (s *Store) save(ctx context.Context, sess *session.Session) error {
	if sess.ID == "" {
		return errors.New("save: session has no ID")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	metaJSON := ""
	if len(sess.Metadata) > 0 {
		b, err := json.Marshal(sess.Metadata)
		if err != nil {
			return fmt.Errorf("marshal metadata: %w", err)
		}
		metaJSON = string(b)
	}
	cacheJSON := ""
	if len(sess.GatewayAssetCache) > 0 {
		b, err := json.Marshal(sess.GatewayAssetCache)
		if err != nil {
			return fmt.Errorf("marshal gateway asset cache: %w", err)
		}
		cacheJSON = string(b)
	}
	ledgerJSON := ""
	if len(sess.ReferenceLedger) > 0 {
		b, err := json.Marshal(sess.ReferenceLedger)
		if err != nil {
			return fmt.Errorf("marshal reference ledger: %w", err)
		}
		ledgerJSON = string(b)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO sessions (id, model, summary, prompt_tokens, context_window, compact_threshold, workspace_path, workspace_root, workspace_rel, workspace_ext_id, name, created_at, updated_at, metadata, gateway_assets, reference_ledger)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			model=excluded.model, summary=excluded.summary, prompt_tokens=excluded.prompt_tokens,
			context_window=excluded.context_window, compact_threshold=excluded.compact_threshold,
			workspace_path=excluded.workspace_path, workspace_root=excluded.workspace_root,
			workspace_rel=excluded.workspace_rel, workspace_ext_id=excluded.workspace_ext_id,
			name=excluded.name, updated_at=excluded.updated_at, metadata=excluded.metadata,
			gateway_assets=excluded.gateway_assets, reference_ledger=excluded.reference_ledger`,
		sess.ID, sess.Model, sess.Summary, sess.PromptTokens, sess.ContextWindow, sess.CompactThreshold,
		sess.WorkspacePath, sess.Workspace.Root, sess.Workspace.Rel, sess.Workspace.ExtID,
		sess.Name, formatTime(sess.CreatedAt), formatTime(sess.UpdatedAt), metaJSON, cacheJSON, ledgerJSON); err != nil {
		return fmt.Errorf("save session row: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `DELETE FROM messages WHERE session_id=?`, sess.ID); err != nil {
		return err
	}
	for i, m := range sess.Messages {
		toolCalls := ""
		if len(m.ToolCalls) > 0 {
			b, err := json.Marshal(m.ToolCalls)
			if err != nil {
				return fmt.Errorf("marshal tool_calls: %w", err)
			}
			toolCalls = string(b)
		}
		assetRefs := ""
		if len(m.Assets) > 0 {
			b, err := json.Marshal(m.Assets)
			if err != nil {
				return fmt.Errorf("marshal message assets: %w", err)
			}
			assetRefs = string(b)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO messages (session_id, seq, role, content, tool_calls, tool_call_id, assets)
			VALUES (?, ?, ?, ?, ?, ?, ?)`,
			sess.ID, i, string(m.Role), m.Content, toolCalls, m.ToolCallID, assetRefs); err != nil {
			return fmt.Errorf("save message %d: %w", i, err)
		}
	}

	if _, err := tx.ExecContext(ctx, `DELETE FROM compactions WHERE session_id=?`, sess.ID); err != nil {
		return err
	}
	for i, c := range sess.Compactions {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO compactions (session_id, seq, before_tokens, after_tokens, saved_tokens, compression_ratio, summary_chars, compacted_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			sess.ID, i, c.BeforeTokens, c.AfterTokens, c.SavedTokens, c.CompressionRatio, c.SummaryChars,
			formatTime(c.CompactedAt)); err != nil {
			return fmt.Errorf("save compaction %d: %w", i, err)
		}
	}

	return tx.Commit()
}

func (s *Store) Load(ctx context.Context, id string) (*session.Session, error) {
	var sess session.Session
	var createdAt, updatedAt, metaJSON, cacheJSON, ledgerJSON, name string
	err := s.db.QueryRowContext(ctx, `
		SELECT id, model, summary, prompt_tokens, context_window, compact_threshold, COALESCE(workspace_path, ''),
		       COALESCE(workspace_root, ''), COALESCE(workspace_rel, ''), COALESCE(workspace_ext_id, ''),
		       COALESCE(name, ''), created_at, updated_at, COALESCE(metadata, ''), COALESCE(gateway_assets, ''), COALESCE(reference_ledger, '')
		FROM sessions WHERE id=?`, id).
		Scan(&sess.ID, &sess.Model, &sess.Summary, &sess.PromptTokens, &sess.ContextWindow,
			&sess.CompactThreshold, &sess.WorkspacePath,
			&sess.Workspace.Root, &sess.Workspace.Rel, &sess.Workspace.ExtID,
			&name, &createdAt, &updatedAt, &metaJSON, &cacheJSON, &ledgerJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("session %q not found", id)
	}
	if err != nil {
		return nil, err
	}
	sess.CreatedAt = parseTime(createdAt)
	sess.UpdatedAt = parseTime(updatedAt)
	sess.Name = name
	// workspace_path is the (frozen) display hint for the portable ref.
	sess.Workspace.AbsHint = sess.WorkspacePath
	sess.Metadata = map[string]any{}
	if metaJSON != "" {
		if err := json.Unmarshal([]byte(metaJSON), &sess.Metadata); err != nil {
			return nil, fmt.Errorf("unmarshal metadata: %w", err)
		}
	}
	if cacheJSON != "" {
		if err := json.Unmarshal([]byte(cacheJSON), &sess.GatewayAssetCache); err != nil {
			return nil, fmt.Errorf("unmarshal gateway asset cache: %w", err)
		}
	}
	if ledgerJSON != "" {
		if err := json.Unmarshal([]byte(ledgerJSON), &sess.ReferenceLedger); err != nil {
			return nil, fmt.Errorf("unmarshal reference ledger: %w", err)
		}
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT role, content, tool_calls, tool_call_id, COALESCE(assets, '') FROM messages WHERE session_id=? ORDER BY seq`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var role, content, toolCalls, toolCallID, assetRefs string
		if err := rows.Scan(&role, &content, &toolCalls, &toolCallID, &assetRefs); err != nil {
			return nil, err
		}
		m := model.Message{Role: model.Role(role), Content: content, ToolCallID: toolCallID}
		if toolCalls != "" {
			if err := json.Unmarshal([]byte(toolCalls), &m.ToolCalls); err != nil {
				return nil, fmt.Errorf("unmarshal tool_calls: %w", err)
			}
		}
		if assetRefs != "" {
			if err := json.Unmarshal([]byte(assetRefs), &m.Assets); err != nil {
				return nil, fmt.Errorf("unmarshal message assets: %w", err)
			}
		}
		sess.Messages = append(sess.Messages, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	crows, err := s.db.QueryContext(ctx, `
		SELECT before_tokens, after_tokens, saved_tokens, compression_ratio, summary_chars, compacted_at
		FROM compactions WHERE session_id=? ORDER BY seq`, id)
	if err != nil {
		return nil, err
	}
	defer crows.Close()
	for crows.Next() {
		var c session.CompactionStats
		var at string
		if err := crows.Scan(&c.BeforeTokens, &c.AfterTokens, &c.SavedTokens, &c.CompressionRatio,
			&c.SummaryChars, &at); err != nil {
			return nil, err
		}
		c.CompactedAt = parseTime(at)
		sess.Compactions = append(sess.Compactions, c)
	}
	if err := crows.Err(); err != nil {
		return nil, err
	}

	return &sess, nil
}

func (s *Store) List(ctx context.Context) ([]session.Meta, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT s.id, s.model, s.prompt_tokens, s.updated_at, COALESCE(s.workspace_path, ''),
		       COALESCE(s.workspace_root, ''), COALESCE(s.workspace_rel, ''), COALESCE(s.workspace_ext_id, ''), COALESCE(s.name, ''),
		       COALESCE(s.metadata, ''),
		       (SELECT COUNT(*) FROM messages m WHERE m.session_id = s.id),
		       (SELECT COALESCE((SELECT content FROM messages m WHERE m.session_id = s.id AND m.role = 'user' ORDER BY m.seq LIMIT 1), '')),
		       (SELECT COUNT(*) FROM compactions c WHERE c.session_id = s.id AND c.after_tokens >= 0),
		       (SELECT COALESCE(SUM(c.saved_tokens), 0) FROM compactions c WHERE c.session_id = s.id AND c.after_tokens >= 0),
		       (SELECT COALESCE(MAX(c.compacted_at), '') FROM compactions c WHERE c.session_id = s.id AND c.after_tokens >= 0)
		FROM sessions s
		ORDER BY s.updated_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []session.Meta
	for rows.Next() {
		var m session.Meta
		var updatedAt, lastCompacted, metadata string
		if err := rows.Scan(&m.ID, &m.Model, &m.PromptTokens, &updatedAt, &m.WorkspacePath,
			&m.Workspace.Root, &m.Workspace.Rel, &m.Workspace.ExtID, &m.Name, &metadata,
			&m.MessageCount, &m.Title, &m.Compactions, &m.TotalSaved, &lastCompacted); err != nil {
			return nil, err
		}
		m.Workspace.AbsHint = m.WorkspacePath
		m.UpdatedAt = parseTime(updatedAt)
		m.LastCompacted = parseTime(lastCompacted)
		m.TurnStatus, m.PausedAt = turnLifecycleFromMetadata(metadata)
		out = append(out, m)
	}
	return out, rows.Err()
}

// turnLifecycleFromMetadata extracts the turn_status and paused_at fields from a
// session's persisted metadata JSON so List can surface them on Meta without
// loading the whole session (v1.2 §3.2). A blank or unparseable blob yields the
// zero values — a session with no lifecycle metadata is simply not paused.
func turnLifecycleFromMetadata(metadata string) (status string, pausedAt int64) {
	if metadata == "" {
		return "", 0
	}
	var meta struct {
		TurnStatus string  `json:"turn_status"`
		PausedAt   float64 `json:"paused_at"`
	}
	if err := json.Unmarshal([]byte(metadata), &meta); err != nil {
		return "", 0
	}
	return meta.TurnStatus, int64(meta.PausedAt)
}

func (s *Store) Stats(ctx context.Context) (session.Stats, error) {
	var st session.Stats
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sessions`).Scan(&st.Sessions); err != nil {
		return session.Stats{}, err
	}
	row := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*),
		       COALESCE(AVG(before_tokens), 0),
		       COALESCE(AVG(after_tokens), 0),
		       COALESCE(AVG(saved_tokens), 0),
		       COALESCE(AVG(compression_ratio), 0),
		       COALESCE(AVG(summary_chars), 0),
		       COALESCE(MAX(compression_ratio), 0),
		       COALESCE(MIN(compression_ratio), 0)
		FROM compactions WHERE after_tokens >= 0`)
	if err := row.Scan(&st.Compactions, &st.AvgBefore, &st.AvgAfter, &st.AvgSaved,
		&st.AvgRatio, &st.AvgSummaryChars, &st.MaxRatio, &st.MinRatio); err != nil {
		return session.Stats{}, err
	}
	_ = s.db.QueryRowContext(ctx, `
		SELECT COALESCE(MAX(prompt_tokens), 0), compact_threshold
		FROM sessions
		ORDER BY prompt_tokens DESC
		LIMIT 1`).Scan(&st.MaxPromptTokens, &st.MaxCompactThreshold)
	return st, nil
}

func (s *Store) RecordRequest(ctx context.Context, r session.RequestRecord) error {
	trace := ""
	if len(r.Trace) > 0 {
		b, err := json.Marshal(r.Trace)
		if err != nil {
			return fmt.Errorf("marshal request trace: %w", err)
		}
		trace = string(b)
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO requests (at, model, prompt_tokens, cached_prompt_tokens, completion_tokens, attempts, retries, timed_out, success, error_class, latency_ms, trace)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		formatTime(r.At), r.Model, r.PromptTokens, r.CachedPromptTokens, r.CompletionTokens, r.Attempts, r.Retries,
		boolToInt(r.TimedOut), boolToInt(r.Success), r.ErrorClass, r.LatencyMs, trace)
	return err
}

func (s *Store) RecordEvent(ctx context.Context, e session.EventRecord) (int64, error) {
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO session_events (session_id, turn_id, kind, at, payload)
		VALUES (?, ?, ?, ?, ?)`,
		e.SessionID, e.TurnID, e.Kind, formatTime(e.At), string(e.Payload))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId() // the rowid is the wire seq (v1.2 §4)
}

func (s *Store) SessionEvents(ctx context.Context, sessionID string) ([]session.EventRecord, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, session_id, turn_id, kind, at, COALESCE(payload, '')
		FROM session_events WHERE session_id=? ORDER BY at ASC, id ASC`, sessionID)
	if err != nil {
		return nil, err
	}
	return scanEventRows(rows)
}

// SessionEventsSince returns events with rowid (seq) greater than sinceSeq, in seq
// order — the incremental catch-up for a reconnecting client (v1.2 §4). Ordering
// by id (not at) makes "seq > since" an exact, gap-free tail of what the client
// already holds.
func (s *Store) SessionEventsSince(ctx context.Context, sessionID string, sinceSeq int64) ([]session.EventRecord, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, session_id, turn_id, kind, at, COALESCE(payload, '')
		FROM session_events WHERE session_id=? AND id > ? ORDER BY id ASC`, sessionID, sinceSeq)
	if err != nil {
		return nil, err
	}
	return scanEventRows(rows)
}

// scanEventRows reads event rows (id, session_id, turn_id, kind, at, payload) into
// EventRecords, mapping the rowid onto Seq. It closes rows.
func scanEventRows(rows *sql.Rows) ([]session.EventRecord, error) {
	defer rows.Close()
	var out []session.EventRecord
	for rows.Next() {
		var e session.EventRecord
		var at, payload string
		if err := rows.Scan(&e.Seq, &e.SessionID, &e.TurnID, &e.Kind, &at, &payload); err != nil {
			return nil, err
		}
		e.At = parseTime(at)
		if payload != "" {
			e.Payload = json.RawMessage(payload)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *Store) RecentEventsByKind(ctx context.Context, kind string, limit int) ([]session.EventRecord, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT session_id, turn_id, kind, at, COALESCE(payload, '')
		FROM session_events WHERE kind=? ORDER BY id DESC LIMIT ?`, kind, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []session.EventRecord
	for rows.Next() {
		var e session.EventRecord
		var at, payload string
		if err := rows.Scan(&e.SessionID, &e.TurnID, &e.Kind, &at, &payload); err != nil {
			return nil, err
		}
		e.At = parseTime(at)
		if payload != "" {
			e.Payload = json.RawMessage(payload)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// SessionEventAttention returns a cursor-bounded durable head projection. The
// terminal subquery deliberately excludes turn_paused: a paused turn remains
// active and resumable.
func (s *Store) SessionEventAttention(ctx context.Context, sinceSequence int64) (session.EventAttentionSnapshot, error) {
	if sinceSequence < 0 {
		sinceSequence = 0
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return session.EventAttentionSnapshot{}, err
	}
	defer func() { _ = tx.Rollback() }()

	var cursor int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(id), 0) FROM session_events`).Scan(&cursor); err != nil {
		return session.EventAttentionSnapshot{}, err
	}
	rows, err := tx.QueryContext(ctx, `
		WITH changed_sessions AS (
			SELECT DISTINCT session_id
			FROM session_events
			WHERE id > ? AND id <= ?
		), heads AS (
			SELECT e.session_id, MAX(e.id) AS last_sequence
			FROM session_events e
			JOIN changed_sessions c ON c.session_id = e.session_id
			WHERE e.id <= ?
			GROUP BY e.session_id
		), terminal_heads AS (
			SELECT e.session_id, MAX(e.id) AS terminal_sequence
			FROM session_events e
			JOIN changed_sessions c ON c.session_id = e.session_id
			WHERE e.id <= ? AND e.kind IN ('turn_finished', 'turn_failed', 'turn_cancelled')
			GROUP BY e.session_id
		)
		SELECT h.session_id, h.last_sequence,
		       latest.id, latest.turn_id, latest.kind, latest.at, COALESCE(latest.payload, ''),
		       t.id, t.turn_id, t.kind, t.at, COALESCE(t.payload, '')
		FROM heads h
		JOIN session_events latest ON latest.id = h.last_sequence
		LEFT JOIN terminal_heads th ON th.session_id = h.session_id
		LEFT JOIN session_events t ON t.id = th.terminal_sequence
		ORDER BY h.session_id ASC`, sinceSequence, cursor, cursor, cursor)
	if err != nil {
		return session.EventAttentionSnapshot{}, err
	}
	defer rows.Close()

	var out []session.EventAttention
	for rows.Next() {
		var head session.EventAttention
		var latestSeq int64
		var latestTurnID, latestKind, latestAt, latestPayload string
		var seq sql.NullInt64
		var turnID, kind, at, payload sql.NullString
		if err := rows.Scan(
			&head.SessionID, &head.LastSequence,
			&latestSeq, &latestTurnID, &latestKind, &latestAt, &latestPayload,
			&seq, &turnID, &kind, &at, &payload,
		); err != nil {
			return session.EventAttentionSnapshot{}, err
		}
		latest := session.EventRecord{
			Seq:       latestSeq,
			SessionID: head.SessionID,
			TurnID:    latestTurnID,
			Kind:      latestKind,
			At:        parseTime(latestAt),
		}
		if latestPayload != "" {
			latest.Payload = json.RawMessage(latestPayload)
		}
		head.LatestEvent = &latest
		if seq.Valid {
			record := session.EventRecord{
				Seq:       seq.Int64,
				SessionID: head.SessionID,
				TurnID:    turnID.String,
				Kind:      kind.String,
				At:        parseTime(at.String),
			}
			if payload.String != "" {
				record.Payload = json.RawMessage(payload.String)
			}
			head.LatestTerminal = &record
		}
		out = append(out, head)
	}
	if err := rows.Err(); err != nil {
		return session.EventAttentionSnapshot{}, err
	}
	if err := tx.Commit(); err != nil {
		return session.EventAttentionSnapshot{}, err
	}
	return session.EventAttentionSnapshot{LastSequence: cursor, Sessions: out}, nil
}

func (s *Store) TokenUsageByModel(ctx context.Context) ([]session.ModelUsage, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT model, COUNT(*), COALESCE(SUM(prompt_tokens), 0), COALESCE(SUM(cached_prompt_tokens), 0), COALESCE(SUM(completion_tokens), 0)
		FROM requests
		GROUP BY model
		ORDER BY SUM(prompt_tokens) + SUM(completion_tokens) DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []session.ModelUsage
	for rows.Next() {
		var u session.ModelUsage
		if err := rows.Scan(&u.Model, &u.Requests, &u.PromptTokens, &u.CachedPromptTokens, &u.CompletionTokens); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

func (s *Store) RecentRequests(ctx context.Context, limit int) ([]session.RequestRecord, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT at, model, prompt_tokens, attempts, retries, timed_out, success, error_class, latency_ms, COALESCE(trace, '')
		FROM requests ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []session.RequestRecord
	for rows.Next() {
		var r session.RequestRecord
		var at, trace string
		var timedOut, success int
		if err := rows.Scan(&at, &r.Model, &r.PromptTokens, &r.Attempts, &r.Retries,
			&timedOut, &success, &r.ErrorClass, &r.LatencyMs, &trace); err != nil {
			return nil, err
		}
		r.At = parseTime(at)
		r.TimedOut = timedOut != 0
		r.Success = success != 0
		if trace != "" {
			if err := json.Unmarshal([]byte(trace), &r.Trace); err != nil {
				return nil, fmt.Errorf("unmarshal request trace: %w", err)
			}
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) ProviderStats(ctx context.Context) (session.ProviderStats, error) {
	var st session.ProviderStats
	row := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*),
		       COALESCE(SUM(success), 0),
		       COALESCE(SUM(CASE WHEN success = 0 THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(timed_out), 0),
		       COALESCE(SUM(retries), 0),
		       COALESCE(AVG(latency_ms), 0),
		       COALESCE(MAX(latency_ms), 0)
		FROM requests`)
	if err := row.Scan(&st.Requests, &st.Successes, &st.Failures, &st.Timeouts,
		&st.Retries, &st.AvgLatencyMs, &st.MaxLatencyMs); err != nil {
		return session.ProviderStats{}, err
	}

	rows, err := s.db.QueryContext(ctx, `SELECT latency_ms FROM requests ORDER BY latency_ms`)
	if err != nil {
		return session.ProviderStats{}, err
	}
	defer rows.Close()
	var lat []int64
	for rows.Next() {
		var ms int64
		if err := rows.Scan(&ms); err != nil {
			return session.ProviderStats{}, err
		}
		lat = append(lat, ms)
	}
	if err := rows.Err(); err != nil {
		return session.ProviderStats{}, err
	}
	st.P50LatencyMs = session.Percentile(lat, 50)
	st.P95LatencyMs = session.Percentile(lat, 95)
	st.P99LatencyMs = session.Percentile(lat, 99)
	st.Histogram = session.Histogram(lat)
	return st, nil
}

func (s *Store) Delete(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, q := range []string{
		`DELETE FROM messages WHERE session_id=?`,
		`DELETE FROM compactions WHERE session_id=?`,
		`DELETE FROM session_events WHERE session_id=?`,
		`DELETE FROM sessions WHERE id=?`,
	} {
		if _, err := tx.ExecContext(ctx, q, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// UpdateName changes just the display name of a session without loading/saving
// the full session. Used by async title generation and the PATCH rename endpoint.
func (s *Store) UpdateName(ctx context.Context, id string, name string) error {
	err := s.updateName(ctx, id, name)
	if err != nil && isReadonlyErr(err) {
		if rerr := s.reopen(); rerr == nil {
			err = s.updateName(ctx, id, name)
		}
	}
	return err
}

func (s *Store) updateName(ctx context.Context, id string, name string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE sessions SET name = ?, updated_at = ? WHERE id = ?`,
		name, formatTime(time.Now()), id)
	return err
}

// ── Time helpers ──────────────────────────────────────────────────────────

// tsLayout is RFC3339 in UTC with a FIXED 9-digit fractional second. Fixed width
// matters: List does ORDER BY updated_at on the text column, and only a
// fixed-width layout sorts lexically the same as chronologically (RFC3339Nano
// trims trailing zeros, so "…00Z" would wrongly sort after "…00.5Z").
const tsLayout = "2006-01-02T15:04:05.000000000Z07:00"

func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(tsLayout)
}

func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
