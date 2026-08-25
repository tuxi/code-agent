package automation

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver, registered as "sqlite" (no cgo)
)

// sqlStore is the SQLite-backed Store. Automations are process-wide and
// cross-workspace, so they live in a dedicated DB at StateDir()/automations.db —
// NOT the per-project hashed session DB (mirrors workbuddy's single workbuddy.db).
type sqlStore struct {
	db *sql.DB
}

// OpenStore opens (creating if needed) the automation store at path.
func OpenStore(path string) (*sqlStore, error) {
	if err := os.MkdirAll(dirOf(path), 0o755); err != nil {
		return nil, fmt.Errorf("create automation store dir: %w", err)
	}
	dsn := path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open automation store %q: %w", path, err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(automationSchema); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate automation store: %w", err)
	}
	// Additive migrations: columns added after the table first shipped. ADD COLUMN
	// is idempotent — "duplicate column" just means it already applied.
	for _, stmt := range []string{
		`ALTER TABLE automations ADD COLUMN retry_count INTEGER NOT NULL DEFAULT 0`,
	} {
		if _, err := db.Exec(stmt); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			db.Close()
			return nil, fmt.Errorf("migrate automation store (%s): %w", stmt, err)
		}
	}
	return &sqlStore{db: db}, nil
}

func dirOf(p string) string {
	if i := strings.LastIndexByte(p, os.PathSeparator); i >= 0 {
		return p[:i]
	}
	return "."
}

const automationSchema = `
CREATE TABLE IF NOT EXISTS automations (
	id                     TEXT PRIMARY KEY,
	name                   TEXT NOT NULL,
	prompt                 TEXT NOT NULL,
	status                 TEXT NOT NULL,
	schedule_type          TEXT NOT NULL DEFAULT 'recurring',
	rrule                  TEXT NOT NULL DEFAULT '',
	scheduled_at           TEXT,
	timezone               TEXT NOT NULL,
	mode_exec              TEXT NOT NULL DEFAULT 'standalone',
	session_id             TEXT,
	cwds                   TEXT NOT NULL DEFAULT '[]',
	model_id               TEXT,
	skills_json            TEXT NOT NULL DEFAULT '[]',
	connector_ids_json     TEXT NOT NULL DEFAULT '[]',
	permission_mode        TEXT,
	valid_from             TEXT,
	valid_until            TEXT,
	created_from_workspace TEXT,
	last_run_at            INTEGER,
	next_run_at            INTEGER,
	run_count              INTEGER NOT NULL DEFAULT 0,
	last_status            TEXT,
	retry_count            INTEGER NOT NULL DEFAULT 0,
	created_at             INTEGER NOT NULL,
	updated_at             INTEGER NOT NULL,
	deleted_at             INTEGER
);
CREATE INDEX IF NOT EXISTS idx_automations_next_run ON automations(status, deleted_at, next_run_at);

CREATE TABLE IF NOT EXISTS automation_runtime_state (
	automation_id        TEXT PRIMARY KEY,
	last_run_at          INTEGER,
	last_error           TEXT,
	running              INTEGER NOT NULL DEFAULT 0,
	running_started_at   INTEGER,
	running_turn_id      TEXT
);

CREATE TABLE IF NOT EXISTS automation_runs (
	id            TEXT PRIMARY KEY,
	automation_id TEXT NOT NULL,
	session_id    TEXT,
	status        TEXT NOT NULL,
	read_at       INTEGER,
	thread_title  TEXT,
	source_cwd    TEXT,
	result_success INTEGER,
	result_summary TEXT,
	created_at    INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_automation_runs_auto ON automation_runs(automation_id, created_at);
`

// Close closes the database.
func (s *sqlStore) Close() error { return s.db.Close() }

// Create inserts a new automation and computes its first NextRunAt.
func (s *sqlStore) Create(ctx context.Context, a Automation) (Automation, error) {
	now := time.Now().UTC()
	a.CreatedAt = now
	a.UpdatedAt = now
	if a.Status == "" {
		a.Status = StatusActive
	}
	if a.ScheduleType == "" {
		a.ScheduleType = ScheduleRecurring
	}
	if a.ModeExec == "" {
		a.ModeExec = ModeStandalone
	}
	if a.ID == "" {
		a.ID = newID()
	}
	next, err := computeNextRunAt(a, now)
	if err != nil {
		return Automation{}, err
	}
	a.NextRunAt = next

	_, err = s.db.ExecContext(ctx, `
		INSERT INTO automations (
			id, name, prompt, status, schedule_type, rrule, scheduled_at, timezone, mode_exec,
			session_id, cwds, model_id, skills_json, connector_ids_json, permission_mode,
			valid_from, valid_until, created_from_workspace,
			last_run_at, next_run_at, run_count, last_status, retry_count, created_at, updated_at, deleted_at
		) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		a.ID, a.Name, a.Prompt, string(a.Status), string(a.ScheduleType), a.RRule, fmtTime(a.ScheduledAt),
		a.Timezone, string(a.ModeExec), a.SessionID, jsonStr(a.CWDs), a.ModelID, jsonStr(a.Skills),
		jsonStr(a.Connectors), a.PermissionMode, fmtTime(a.ValidFrom), fmtTime(a.ValidUntil),
		a.CreatedFromWorkspace, epochMS(a.LastRunAt), epochMS(a.NextRunAt), a.RunCount, a.LastStatus,
		a.RetryCount, epochMS(a.CreatedAt), epochMS(a.UpdatedAt), epochMSOrNil(a.DeletedAt))
	if err != nil {
		return Automation{}, fmt.Errorf("create automation: %w", err)
	}
	return a, nil
}

// Get returns one automation, including soft-deleted (so view can inspect it).
func (s *sqlStore) Get(ctx context.Context, id string) (Automation, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, name, prompt, status, schedule_type, rrule, COALESCE(scheduled_at,''), timezone,
		       mode_exec, COALESCE(session_id,''), cwds, COALESCE(model_id,''), skills_json,
		       connector_ids_json, COALESCE(permission_mode,''), COALESCE(valid_from,''), COALESCE(valid_until,''),
		       COALESCE(created_from_workspace,''), last_run_at, next_run_at, run_count, COALESCE(last_status,''),
		       retry_count, created_at, updated_at, deleted_at
		FROM automations WHERE id=?`, id)
	a, err := scanAutomation(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Automation{}, fmt.Errorf("automation %q not found", id)
	}
	return a, err
}

// List returns non-deleted automations, newest first.
func (s *sqlStore) List(ctx context.Context) ([]Automation, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, name, prompt, status, schedule_type, rrule, COALESCE(scheduled_at,''), timezone,
		       mode_exec, COALESCE(session_id,''), cwds, COALESCE(model_id,''), skills_json,
		       connector_ids_json, COALESCE(permission_mode,''), COALESCE(valid_from,''), COALESCE(valid_until,''),
		       COALESCE(created_from_workspace,''), last_run_at, next_run_at, run_count, COALESCE(last_status,''),
		       retry_count, created_at, updated_at, deleted_at
		FROM automations WHERE deleted_at IS NULL
		ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Automation
	for rows.Next() {
		a, err := scanAutomation(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

type scanner interface {
	Scan(dest ...any) error
}

// scanAutomation reads one Automation from a row/scanner. Column order must match
// the SELECT in Get and List exactly.
func scanAutomation(sc scanner) (Automation, error) {
	var a Automation
	var status, scheduleType, rrule, scheduledAt, timezone, modeExec, sessionID, cwds, modelID, skills, connectors, permissionMode, validFrom, validUntil, createdFromWorkspace, lastStatus string
	var lastRunAt, nextRunAt, createdAt, updatedAt, deletedAt sql.NullInt64
	var runCount, retryCount int64
	err := sc.Scan(&a.ID, &a.Name, &a.Prompt, &status, &scheduleType, &rrule, &scheduledAt, &timezone,
		&modeExec, &sessionID, &cwds, &modelID, &skills, &connectors, &permissionMode, &validFrom,
		&validUntil, &createdFromWorkspace, &lastRunAt, &nextRunAt, &runCount, &lastStatus, &retryCount,
		&createdAt, &updatedAt, &deletedAt)
	if err != nil {
		return Automation{}, err
	}
	a.Status = Status(status)
	a.ScheduleType = ScheduleType(scheduleType)
	a.RRule = rrule
	a.ScheduledAt = parseTime(scheduledAt)
	a.Timezone = timezone
	a.ModeExec = RunMode(modeExec)
	a.SessionID = sessionID
	a.CWDs = parseJSONStrings(cwds)
	a.ModelID = modelID
	a.Skills = parseJSONStrings(skills)
	a.Connectors = parseJSONStrings(connectors)
	a.PermissionMode = permissionMode
	a.ValidFrom = parseTime(validFrom)
	a.ValidUntil = parseTime(validUntil)
	a.CreatedFromWorkspace = createdFromWorkspace
	a.LastRunAt = time.UnixMilli(lastRunAt.Int64).UTC()
	a.NextRunAt = time.UnixMilli(nextRunAt.Int64).UTC()
	a.RunCount = runCount
	a.LastStatus = lastStatus
	a.RetryCount = int(retryCount)
	a.CreatedAt = time.UnixMilli(createdAt.Int64).UTC()
	a.UpdatedAt = time.UnixMilli(updatedAt.Int64).UTC()
	a.DeletedAt = time.Time{}
	if deletedAt.Valid && deletedAt.Int64 != 0 {
		a.DeletedAt = time.UnixMilli(deletedAt.Int64).UTC()
	}
	return a, nil
}

// Update applies a partial update; only non-zero patch fields change the row. If
// schedule-affecting fields changed, NextRunAt is recomputed. Returns the updated
// automation.
func (s *sqlStore) Update(ctx context.Context, id string, patch AutomationPatch) (Automation, error) {
	existing, err := s.Get(ctx, id)
	if err != nil {
		return Automation{}, err
	}

	// Build the update dynamically. Track whether schedule inputs changed so we
	// know if next_run_at must be recomputed.
	scheduleChanged := patch.RRule != nil || patch.ScheduleType != nil || patch.ScheduledAt != nil || patch.Timezone != nil

	if patch.Name != nil {
		existing.Name = *patch.Name
	}
	if patch.Prompt != nil {
		existing.Prompt = *patch.Prompt
	}
	if patch.Status != nil {
		existing.Status = *patch.Status
	}
	if patch.ScheduleType != nil {
		existing.ScheduleType = *patch.ScheduleType
	}
	if patch.RRule != nil {
		existing.RRule = *patch.RRule
	}
	if patch.ScheduledAt != nil {
		existing.ScheduledAt = *patch.ScheduledAt
	}
	if patch.Timezone != nil {
		existing.Timezone = *patch.Timezone
	}
	if patch.ModeExec != nil {
		existing.ModeExec = *patch.ModeExec
	}
	if patch.SessionID != nil {
		existing.SessionID = *patch.SessionID
	}
	if patch.CWDs != nil {
		existing.CWDs = *patch.CWDs
	}
	if patch.ModelID != nil {
		existing.ModelID = *patch.ModelID
	}
	if patch.Skills != nil {
		existing.Skills = *patch.Skills
	}
	if patch.Connectors != nil {
		existing.Connectors = *patch.Connectors
	}
	if patch.PermissionMode != nil {
		existing.PermissionMode = *patch.PermissionMode
	}
	if patch.ValidFrom != nil {
		existing.ValidFrom = *patch.ValidFrom
	}
	if patch.ValidUntil != nil {
		existing.ValidUntil = *patch.ValidUntil
	}
	if patch.CreatedFromWorkspace != nil {
		existing.CreatedFromWorkspace = *patch.CreatedFromWorkspace
	}
	if patch.LastRunAt != nil {
		existing.LastRunAt = *patch.LastRunAt
	}
	if patch.LastStatus != nil {
		existing.LastStatus = *patch.LastStatus
	}
	if patch.RetryCount != nil {
		existing.RetryCount = *patch.RetryCount
	}

	existing.UpdatedAt = time.Now().UTC()
	if scheduleChanged {
		next, err := computeNextRunAt(existing, existing.LastRunAt)
		if err != nil {
			return Automation{}, err
		}
		existing.NextRunAt = next
	}

	if _, err := s.db.ExecContext(ctx, `
		UPDATE automations SET
			name=?, prompt=?, status=?, schedule_type=?, rrule=?, scheduled_at=?, timezone=?, mode_exec=?,
			session_id=?, cwds=?, model_id=?, skills_json=?, connector_ids_json=?, permission_mode=?,
			valid_from=?, valid_until=?, created_from_workspace=?, next_run_at=?, last_run_at=?, last_status=?, retry_count=?, updated_at=?
		WHERE id=?`,
		existing.Name, existing.Prompt, string(existing.Status), string(existing.ScheduleType), existing.RRule,
		fmtTime(existing.ScheduledAt), existing.Timezone, string(existing.ModeExec), existing.SessionID,
		jsonStr(existing.CWDs), existing.ModelID, jsonStr(existing.Skills), jsonStr(existing.Connectors),
		existing.PermissionMode, fmtTime(existing.ValidFrom), fmtTime(existing.ValidUntil),
		existing.CreatedFromWorkspace, epochMS(existing.NextRunAt), epochMS(existing.LastRunAt),
		existing.LastStatus, existing.RetryCount, epochMS(existing.UpdatedAt), id); err != nil {
		return Automation{}, fmt.Errorf("update automation: %w", err)
	}
	return existing, nil
}

// Delete soft-deletes an automation.
func (s *sqlStore) Delete(ctx context.Context, id string) error {
	now := time.Now().UTC()
	res, err := s.db.ExecContext(ctx, `UPDATE automations SET deleted_at=?, updated_at=? WHERE id=? AND deleted_at IS NULL`,
		epochMS(now), epochMS(now), id)
	if err != nil {
		return fmt.Errorf("delete automation: %w", err)
	}
	if rf, _ := res.RowsAffected(); rf == 0 {
		// Either it did not exist or was already deleted. Treat as idempotent no-op.
		return nil
	}
	return nil
}

// NextDueAt returns ACTIVE, non-deleted automations whose NextRunAt <= now, in
// ascending next_run_at order.
func (s *sqlStore) NextDueAt(ctx context.Context, now time.Time) ([]Automation, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, name, prompt, status, schedule_type, rrule, COALESCE(scheduled_at,''), timezone,
		       mode_exec, COALESCE(session_id,''), cwds, COALESCE(model_id,''), skills_json,
		       connector_ids_json, COALESCE(permission_mode,''), COALESCE(valid_from,''), COALESCE(valid_until,''),
		       COALESCE(created_from_workspace,''), last_run_at, next_run_at, run_count, COALESCE(last_status,''),
		       retry_count, created_at, updated_at, deleted_at
		FROM automations
		WHERE deleted_at IS NULL AND status='ACTIVE' AND next_run_at IS NOT NULL AND next_run_at <= ?
		ORDER BY next_run_at ASC`, epochMS(now))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Automation
	for rows.Next() {
		a, err := scanAutomation(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// SkipDue marks ACTIVE, non-deleted, overdue automations as skipped, recording a
// skipped run for each. This is the R15 daemon-restart reconcile path: overdue (in
// truth unattended) firings are not re-run, they are recorded and dropped. Returns
// the number skipped.
func (s *sqlStore) SkipDue(ctx context.Context, now time.Time) (int, error) {
	due, err := s.NextDueAt(ctx, now)
	if err != nil {
		return 0, err
	}
	for _, a := range due {
		_ = s.RecordRun(ctx, Run{AutomationID: a.ID, Status: RunSkipped, CreatedAt: now})
		// Advance next_run_at so this firing is not picked up again next tick.
		var next time.Time
		if a.ScheduleType == ScheduleRecurring {
			if computed, err := computeNextRunAt(a, now); err == nil {
				next = computed
			}
		}
		_ = s.UpdateNextRunAt(ctx, a.ID, next)
	}
	return len(due), nil
}

// UpdateNextRunAt advances a single automation's next_run_at.
func (s *sqlStore) UpdateNextRunAt(ctx context.Context, id string, next time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE automations SET next_run_at=? WHERE id=?`,
		epochMS(next), id)
	return err
}

// RecordRun inserts one run record.
func (s *sqlStore) RecordRun(ctx context.Context, r Run) error {
	if r.ID == "" {
		r.ID = newID()
	}
	if r.CreatedAt.IsZero() {
		r.CreatedAt = time.Now().UTC()
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO automation_runs (id, automation_id, session_id, status, read_at, thread_title, source_cwd, result_success, result_summary, created_at)
		VALUES (?,?,?,?,?,?,?,?,?,?)`,
		r.ID, r.AutomationID, r.SessionID, r.Status, epochMS(r.ReadAt), r.ThreadTitle, r.SourceCWD,
		boolToInt(r.ResultSuccess), r.ResultSummary, epochMS(r.CreatedAt))
	if err != nil {
		return fmt.Errorf("record run: %w", err)
	}
	return nil
}

// ListRuns returns runs for an automation, newest first.
func (s *sqlStore) ListRuns(ctx context.Context, automationID string, limit int) ([]Run, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, automation_id, COALESCE(session_id,''), status, read_at, COALESCE(thread_title,''), COALESCE(source_cwd,''), result_success, COALESCE(result_summary,''), created_at
		FROM automation_runs WHERE automation_id=? ORDER BY created_at DESC LIMIT ?`, automationID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Run
	for rows.Next() {
		var r Run
		var readAt, createdAt int64
		var sessionID, threadTitle, sourceCWD, resultSummary, status string
		if err := rows.Scan(&r.ID, &r.AutomationID, &sessionID, &status, &readAt, &threadTitle, &sourceCWD, &r.ResultSuccess, &resultSummary, &createdAt); err != nil {
			return nil, err
		}
		r.SessionID = sessionID
		r.Status = status
		r.ThreadTitle = threadTitle
		r.SourceCWD = sourceCWD
		r.ResultSummary = resultSummary
		r.ReadAt = time.UnixMilli(readAt).UTC()
		r.CreatedAt = time.UnixMilli(createdAt).UTC()
		out = append(out, r)
	}
	return out, rows.Err()
}

// MarkRunRead marks a run as read.
func (s *sqlStore) MarkRunRead(ctx context.Context, runID string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE automation_runs SET read_at=? WHERE id=?`,
		epochMS(time.Now().UTC()), runID)
	return err
}

// UpdateRuntimeState upserts the hot mutable state for an automation.
func (s *sqlStore) UpdateRuntimeState(ctx context.Context, st RuntimeState) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO automation_runtime_state (automation_id, last_run_at, last_error, running, running_started_at, running_turn_id)
		VALUES (?,?,?,?,?,?)
		ON CONFLICT(automation_id) DO UPDATE SET
			last_run_at=excluded.last_run_at, last_error=excluded.last_error,
			running=excluded.running, running_started_at=excluded.running_started_at,
			running_turn_id=excluded.running_turn_id`,
		st.AutomationID, epochMS(st.LastRunAt), st.LastError, boolToInt(st.Running),
		epochMS(st.RunningStartedAt), st.RunningTurnID)
	return err
}

// RuntimeState returns the hot state for an automation (zero value if none).
func (s *sqlStore) RuntimeState(ctx context.Context, automationID string) (RuntimeState, error) {
	var st RuntimeState
	var lastRunAt, runningStartedAt int64
	var lastError, runningTurnID string
	var running int
	err := s.db.QueryRowContext(ctx, `
		SELECT automation_id, last_run_at, COALESCE(last_error,''), running, running_started_at, COALESCE(running_turn_id,'')
		FROM automation_runtime_state WHERE automation_id=?`, automationID).
		Scan(&st.AutomationID, &lastRunAt, &lastError, &running, &runningStartedAt, &runningTurnID)
	if errors.Is(err, sql.ErrNoRows) {
		return RuntimeState{}, nil
	}
	if err != nil {
		return RuntimeState{}, err
	}
	st.LastRunAt = time.UnixMilli(lastRunAt).UTC()
	st.LastError = lastError
	st.Running = running != 0
	st.RunningStartedAt = time.UnixMilli(runningStartedAt).UTC()
	st.RunningTurnID = runningTurnID
	return st, nil
}

// --- helpers ---

func newID() string {
	return fmt.Sprintf("auto_%d", time.Now().UnixNano())
}

func epochMS(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

// epochMSOrNil returns nil (SQL NULL) for a zero time, else the epoch ms. Use it
// for columns whose "absent" state must be NULL (e.g. deleted_at, so the
// `deleted_at IS NULL` filter works), not 0.
func epochMSOrNil(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UnixMilli()
}

func fmtTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t
	}
	return time.Time{}
}

func jsonStr(v []string) string {
	if len(v) == 0 {
		return "[]"
	}
	b, _ := json.Marshal(v)
	return string(b)
}

func parseJSONStrings(s string) []string {
	if s == "" {
		return []string{}
	}
	var out []string
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return []string{}
	}
	return out
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
