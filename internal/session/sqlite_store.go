package session

import (
	"code-agent/internal/model"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver, registered as "sqlite" (no cgo)
)

// SQLiteStore persists sessions in a single SQLite database. Schema is
// normalized into sessions / messages / compactions so the trace is queryable,
// but Save replaces a session's rows wholesale (a snapshot at the turn
// boundary) — cheap at CLI sizes and trivially consistent, with no delta
// bookkeeping to drift.
type SQLiteStore struct {
	db *sql.DB
}

const schema = `
CREATE TABLE IF NOT EXISTS sessions (
	id                TEXT PRIMARY KEY,
	model             TEXT,
	summary           TEXT,
	prompt_tokens     INTEGER,
	context_window    INTEGER,
	compact_threshold INTEGER,
	created_at        TEXT,
	updated_at        TEXT
);
CREATE TABLE IF NOT EXISTS messages (
	session_id   TEXT,
	seq          INTEGER,
	role         TEXT,
	content      TEXT,
	tool_calls   TEXT,
	tool_call_id TEXT,
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
);`

func NewSQLiteStore(path string) (*SQLiteStore, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite %q: %w", path, err)
	}
	// Serial CLI usage: a single connection sidesteps SQLite "database is locked".
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &SQLiteStore{db: db}, nil
}

func (s *SQLiteStore) Close() error { return s.db.Close() }

func (s *SQLiteStore) Save(ctx context.Context, sess *Session) error {
	if sess.ID == "" {
		return errors.New("save: session has no ID")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO sessions (id, model, summary, prompt_tokens, context_window, compact_threshold, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			model=excluded.model, summary=excluded.summary, prompt_tokens=excluded.prompt_tokens,
			context_window=excluded.context_window, compact_threshold=excluded.compact_threshold,
			updated_at=excluded.updated_at`,
		sess.ID, sess.Model, sess.Summary, sess.PromptTokens, sess.ContextWindow, sess.CompactThreshold,
		formatTime(sess.CreatedAt), formatTime(sess.UpdatedAt)); err != nil {
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
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO messages (session_id, seq, role, content, tool_calls, tool_call_id)
			VALUES (?, ?, ?, ?, ?, ?)`,
			sess.ID, i, string(m.Role), m.Content, toolCalls, m.ToolCallID); err != nil {
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

func (s *SQLiteStore) Load(ctx context.Context, id string) (*Session, error) {
	var sess Session
	var createdAt, updatedAt string
	err := s.db.QueryRowContext(ctx, `
		SELECT id, model, summary, prompt_tokens, context_window, compact_threshold, created_at, updated_at
		FROM sessions WHERE id=?`, id).
		Scan(&sess.ID, &sess.Model, &sess.Summary, &sess.PromptTokens, &sess.ContextWindow,
			&sess.CompactThreshold, &createdAt, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("session %q not found", id)
	}
	if err != nil {
		return nil, err
	}
	sess.CreatedAt = parseTime(createdAt)
	sess.UpdatedAt = parseTime(updatedAt)
	sess.Metadata = map[string]any{}

	rows, err := s.db.QueryContext(ctx, `
		SELECT role, content, tool_calls, tool_call_id FROM messages WHERE session_id=? ORDER BY seq`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var role, content, toolCalls, toolCallID string
		if err := rows.Scan(&role, &content, &toolCalls, &toolCallID); err != nil {
			return nil, err
		}
		m := model.Message{Role: model.Role(role), Content: content, ToolCallID: toolCallID}
		if toolCalls != "" {
			if err := json.Unmarshal([]byte(toolCalls), &m.ToolCalls); err != nil {
				return nil, fmt.Errorf("unmarshal tool_calls: %w", err)
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
		var c CompactionStats
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

func (s *SQLiteStore) List(ctx context.Context) ([]Meta, error) {
	// Per-session compaction aggregates are computed from the compactions table
	// (after_tokens >= 0 = finalized), not denormalized onto sessions — the data
	// already lives there and Save rewrites it wholesale, so there is nothing to
	// keep in sync.
	rows, err := s.db.QueryContext(ctx, `
		SELECT s.id, s.model, s.prompt_tokens, s.updated_at,
		       (SELECT COUNT(*) FROM messages m WHERE m.session_id = s.id),
		       (SELECT COUNT(*) FROM compactions c WHERE c.session_id = s.id AND c.after_tokens >= 0),
		       (SELECT COALESCE(SUM(c.saved_tokens), 0) FROM compactions c WHERE c.session_id = s.id AND c.after_tokens >= 0),
		       (SELECT COALESCE(MAX(c.compacted_at), '') FROM compactions c WHERE c.session_id = s.id AND c.after_tokens >= 0)
		FROM sessions s
		ORDER BY s.updated_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Meta
	for rows.Next() {
		var m Meta
		var updatedAt, lastCompacted string
		if err := rows.Scan(&m.ID, &m.Model, &m.PromptTokens, &updatedAt,
			&m.MessageCount, &m.Compactions, &m.TotalSaved, &lastCompacted); err != nil {
			return nil, err
		}
		m.UpdatedAt = parseTime(updatedAt)
		m.LastCompacted = parseTime(lastCompacted)
		out = append(out, m)
	}
	return out, rows.Err()
}

// Stats aggregates compaction telemetry across all sessions. Only finalized
// compactions (after_tokens >= 0) are counted, so a pending stat awaiting its
// next-call measurement never skews the averages.
func (s *SQLiteStore) Stats(ctx context.Context) (Stats, error) {
	var st Stats
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sessions`).Scan(&st.Sessions); err != nil {
		return Stats{}, err
	}
	// COALESCE so aggregates over an empty set return zeros rather than NULL.
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
		return Stats{}, err
	}
	return st, nil
}

func (s *SQLiteStore) Delete(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, q := range []string{
		`DELETE FROM messages WHERE session_id=?`,
		`DELETE FROM compactions WHERE session_id=?`,
		`DELETE FROM sessions WHERE id=?`,
	} {
		if _, err := tx.ExecContext(ctx, q, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// tsLayout is RFC3339 in UTC with a FIXED 9-digit fractional second. Fixed width
// matters: List does ORDER BY updated_at on the text column, and only a
// fixed-width layout sorts lexically the same as chronologically (RFC3339Nano
// trims trailing zeros, so "…00Z" would wrongly sort after "…00.5Z").
const tsLayout = "2006-01-02T15:04:05.000000000Z07:00"

// Times are stored as RFC3339 strings so the DB is human-readable and timezone
// safe. A zero/unparseable time round-trips to the zero value.
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
