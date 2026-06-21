package session

import (
	"code-agent/internal/model"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver, registered as "sqlite" (no cgo)
)

// SQLiteStore persists sessions in a single SQLite database. Schema is
// normalized into sessions / messages / compactions so the trace is queryable,
// but Save replaces a session's rows wholesale (a snapshot at the turn
// boundary) — cheap at CLI sizes and trivially consistent, with no delta
// bookkeeping to drift.
type SQLiteStore struct {
	db   *sql.DB
	path string
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
CREATE INDEX IF NOT EXISTS idx_session_events_session ON session_events(session_id, id);`

func NewSQLiteStore(path string) (*SQLiteStore, error) {
	s := &SQLiteStore{path: path}
	if err := s.open(); err != nil {
		return nil, err
	}
	return s, nil
}

// open (re)opens the database at s.path and applies the idempotent migrations.
// Used at construction and to recover the connection after the file moved out
// from under it (SQLITE_READONLY_DBMOVED) — see Save.
func (s *SQLiteStore) open() error {
	db, err := sql.Open("sqlite", s.path)
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
func (s *SQLiteStore) reopen() error {
	if s.db != nil {
		_ = s.db.Close()
	}
	return s.open()
}

func (s *SQLiteStore) Close() error { return s.db.Close() }

// Save persists a session as a wholesale snapshot. If the write fails because
// the database file moved out from under the open connection
// (SQLITE_READONLY_DBMOVED — classically a synced folder like iCloud replacing
// the file), it reopens and retries once. Because Save rewrites the full session
// every time, the retry writes the complete state into the current file and
// loses nothing.
func (s *SQLiteStore) Save(ctx context.Context, sess *Session) error {
	err := s.save(ctx, sess)
	if err != nil && isReadonlyErr(err) {
		if rerr := s.reopen(); rerr == nil {
			err = s.save(ctx, sess)
		}
	}
	return err
}

// isReadonlyErr matches SQLite's readonly family, including
// SQLITE_READONLY_DBMOVED (1032), whose message is "attempt to write a readonly
// database".
func isReadonlyErr(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "readonly")
}

func (s *SQLiteStore) save(ctx context.Context, sess *Session) error {
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
	var out []Meta
	for rows.Next() {
		var m Meta
		var updatedAt, lastCompacted string
		if err := rows.Scan(&m.ID, &m.Model, &m.PromptTokens, &updatedAt,
			&m.MessageCount, &m.Title, &m.Compactions, &m.TotalSaved, &lastCompacted); err != nil {
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
	// Query the session with the highest prompt_tokens and its threshold.
	_ = s.db.QueryRowContext(ctx, `
		SELECT COALESCE(MAX(prompt_tokens), 0), compact_threshold
		FROM sessions
		ORDER BY prompt_tokens DESC
		LIMIT 1`).Scan(&st.MaxPromptTokens, &st.MaxCompactThreshold)
	return st, nil
}

// RecordRequest appends one request to the telemetry log. Best-effort by
// convention: callers should not fail a run if this errors.
func (s *SQLiteStore) RecordRequest(ctx context.Context, r RequestRecord) error {
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

// RecordEvent appends one agent event to the per-session event log. Best-effort
// by convention: callers should not fail a run if this errors.
func (s *SQLiteStore) RecordEvent(ctx context.Context, e EventRecord) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO session_events (session_id, turn_id, kind, at, payload)
		VALUES (?, ?, ?, ?, ?)`,
		e.SessionID, e.TurnID, e.Kind, formatTime(e.At), string(e.Payload))
	return err
}

// SessionEvents returns a session's events in emission order (the autoincrement
// id is monotonic with insertion) — the raw stream a replay/analytics pass reads.
func (s *SQLiteStore) SessionEvents(ctx context.Context, sessionID string) ([]EventRecord, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT session_id, turn_id, kind, at, COALESCE(payload, '')
		FROM session_events WHERE session_id=? ORDER BY id`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []EventRecord
	for rows.Next() {
		var e EventRecord
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

// RecentEventsByKind returns the most recent events of one kind across all
// sessions, newest first — the index behind `codeagent tasks`.
func (s *SQLiteStore) RecentEventsByKind(ctx context.Context, kind string, limit int) ([]EventRecord, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT session_id, turn_id, kind, at, COALESCE(payload, '')
		FROM session_events WHERE kind=? ORDER BY id DESC LIMIT ?`, kind, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []EventRecord
	for rows.Next() {
		var e EventRecord
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

// TokenUsageByModel returns per-model token totals (most tokens first) — the
// basis for cost accounting.
func (s *SQLiteStore) TokenUsageByModel(ctx context.Context) ([]ModelUsage, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT model, COUNT(*), COALESCE(SUM(prompt_tokens), 0), COALESCE(SUM(cached_prompt_tokens), 0), COALESCE(SUM(completion_tokens), 0)
		FROM requests
		GROUP BY model
		ORDER BY SUM(prompt_tokens) + SUM(completion_tokens) DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ModelUsage
	for rows.Next() {
		var u ModelUsage
		if err := rows.Scan(&u.Model, &u.Requests, &u.PromptTokens, &u.CachedPromptTokens, &u.CompletionTokens); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// RecentRequests returns the most recent requests (newest first) with their
// per-attempt trace decoded — the data behind `codeagent trace`.
func (s *SQLiteStore) RecentRequests(ctx context.Context, limit int) ([]RequestRecord, error) {
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
	var out []RequestRecord
	for rows.Next() {
		var r RequestRecord
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

// ProviderStats aggregates the request log into transport telemetry.
func (s *SQLiteStore) ProviderStats(ctx context.Context) (ProviderStats, error) {
	var st ProviderStats
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
		return ProviderStats{}, err
	}

	// Latencies for percentiles + histogram. SQLite has no percentile function;
	// at CLI scale, pulling the (sorted) column and computing in Go is exact and
	// simple. Failed/timed-out requests count too — a 120s timeout IS a slow
	// request the distribution should show.
	rows, err := s.db.QueryContext(ctx, `SELECT latency_ms FROM requests ORDER BY latency_ms`)
	if err != nil {
		return ProviderStats{}, err
	}
	defer rows.Close()
	var lat []int64
	for rows.Next() {
		var ms int64
		if err := rows.Scan(&ms); err != nil {
			return ProviderStats{}, err
		}
		lat = append(lat, ms)
	}
	if err := rows.Err(); err != nil {
		return ProviderStats{}, err
	}
	st.P50LatencyMs = percentile(lat, 50)
	st.P95LatencyMs = percentile(lat, 95)
	st.P99LatencyMs = percentile(lat, 99)
	st.Histogram = histogram(lat)
	return st, nil
}

// latencyBuckets defines the histogram's fixed boundaries (exclusive upper, in
// ms). The last is the "and above" bucket.
var latencyBuckets = []LatencyBucket{
	{Label: "<1s", UpperMs: 1000},
	{Label: "1-2s", UpperMs: 2000},
	{Label: "2-5s", UpperMs: 5000},
	{Label: "5-10s", UpperMs: 10000},
	{Label: "10-20s", UpperMs: 20000},
	{Label: "20-30s", UpperMs: 30000},
	{Label: "30s+", UpperMs: math.MaxInt64},
}

// percentile returns the nearest-rank pth percentile of an ascending slice.
func percentile(sortedAsc []int64, p float64) int64 {
	n := len(sortedAsc)
	if n == 0 {
		return 0
	}
	rank := int(math.Ceil(p / 100 * float64(n)))
	if rank < 1 {
		rank = 1
	}
	if rank > n {
		rank = n
	}
	return sortedAsc[rank-1]
}

// histogram buckets latencies into latencyBuckets (counts per bar).
func histogram(latencies []int64) []LatencyBucket {
	out := make([]LatencyBucket, len(latencyBuckets))
	copy(out, latencyBuckets)
	for _, l := range latencies {
		for i := range out {
			if l < out[i].UpperMs {
				out[i].Count++
				break
			}
		}
	}
	return out
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
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
		`DELETE FROM session_events WHERE session_id=?`,
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
