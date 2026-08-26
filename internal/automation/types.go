// Package automation is the time/periodic trigger engine for CodeAgent. It is
// the third, orthogonal component alongside conversation.TurnScheduler (admission
// concurrency control) and internal/jobs (background commands): neither of those
// knows "when to fire" — this package owns the schedule.
//
// An Automation is a persisted definition (what to do + when + where). A Run is
// one actual firing: it always produces a run record, and in standalone mode it
// is a new conversation, while chat mode returns to a fixed session.
package automation

import (
	"context"
	"fmt"
	"time"
)

// ScheduleType distinguishes a one-shot automation from a repeating one. They
// are expressed by different fields (scheduled_at vs rrule) so the semantics are
// unambiguous without runtime type-switching on a single field.
type ScheduleType string

const (
	ScheduleOnce      ScheduleType = "once"
	ScheduleRecurring ScheduleType = "recurring"
)

// Run mode: whether each firing starts a new conversation (standalone), returns
// to a fixed session (chat), or reuses the first firing's conversation (reuse —
// the default for recurring tasks, so a periodic task does not pile up one
// conversation per firing and can benefit from LLM context caching).
type RunMode string

const (
	ModeStandalone RunMode = "standalone"
	ModeChat       RunMode = "chat"
	ModeReuse      RunMode = "reuse"
)

// Status of an automation definition.
type Status string

const (
	StatusActive    Status = "ACTIVE"
	StatusPaused    Status = "PAUSED"
	StatusCompleted Status = "COMPLETED" // once automation that has fired and finished
)

// Run status.
const (
	RunRunning   = "running"
	RunSucceeded = "succeeded"
	RunFailed    = "failed"
	RunSkipped   = "skipped"
	RunCanceled  = "canceled" // the user stopped the firing's conversation
)

// Retry policy for failed firings: a firing that fails is retried up to
// MaxRetries times, RetryInterval apart, then the automation is marked COMPLETED
// so it stops (the user re-enables it from the client to try again).
const (
	MaxRetries    = 3
	RetryInterval = time.Minute
)

// Automation is one persisted automation definition (the stable config).
type Automation struct {
	ID                   string
	Name                 string
	Prompt               string
	Status               Status
	ScheduleType         ScheduleType
	RRule                string    // recurring: RFC5545-style, e.g. "FREQ=DAILY;BYHOUR=16;BYMINUTE=0"
	ScheduledAt          time.Time // once: the exact firing time (zero for recurring)
	Timezone             string    // creation timezone, e.g. "America/Los_Angeles"
	ModeExec             RunMode
	SessionID            string    // chat: the session to return to (empty for standalone)
	CWDs                 []string  // optional target workspaces
	ModelID              string    // optional model to run with
	Skills               []string  // skill names to auto-load at firing
	Connectors           []string  // MCP server names to enable at firing
	PermissionMode       string    // approval tier: "ask" | "auto" | "full"; "" = inherit the workspace tier (see NormalizePermissionMode)
	ValidFrom            time.Time // zero = no lower bound
	ValidUntil           time.Time // zero = no upper bound
	CreatedFromWorkspace string    // workspace of the creating session (standalone fallback)
	LastRunAt            time.Time
	NextRunAt            time.Time // materialized next firing, epoch ms in create timezone
	RunCount             int64
	LastStatus           string
	RetryCount           int // consecutive failed firings; reset on success
	CreatedAt            time.Time
	UpdatedAt            time.Time
	DeletedAt            time.Time // zero = not deleted (soft delete)
}

// RuntimeState is the hot, mutable 1:1 state for an automation. It lives in a
// separate table so each scheduler tick and each firing can update it without
// disturbing the stable definition row, and without locking the definition.
type RuntimeState struct {
	AutomationID     string
	LastRunAt        time.Time
	LastError        string
	Running          bool
	RunningStartedAt time.Time
	RunningTurnID    string
}

// Run is one firing. Unlike workbuddy (which keys runs by thread_id = the
// conversation), we key by an independent run id and store the session id
// separately — so chat mode (multiple firings into the same session) writes one
// run per firing without a primary-key collision (PRD R11a).
type Run struct {
	ID            string
	AutomationID  string
	SessionID     string    // the conversation this firing ran in
	Status        string    // running | succeeded | failed | skipped
	ReadAt        time.Time // zero = unread (inbox indicator)
	ThreadTitle   string
	SourceCWD     string
	ResultSuccess bool
	ResultSummary string
	CreatedAt     time.Time
}

// Dispatcher submits one automation firing as a turn. It is a seam so the
// scheduler loop can be tested against a fake without a live TurnExecutor.
type Dispatcher interface {
	// Dispatch fires the automation once. It must return the turn id of the
	// conversation it ran in, or an error if the firing could not be submitted.
	Dispatch(ctx context.Context, a Automation) (turnID string, err error)
}

// Store is the persistence port for automations, runs, and runtime state.
// It is implemented by the SQLite store in this package and consumed by the
// tools (internal/tools/automation), the HTTP endpoints (internal/server), and
// the scheduler (internal/automation).
type Store interface {
	// Create stores a new automation and computes+returns its first NextRunAt.
	Create(ctx context.Context, a Automation) (Automation, error)
	// Get returns one automation (including soft-deleted, so view can inspect).
	Get(ctx context.Context, id string) (Automation, error)
	// List returns non-deleted automations, newest first.
	List(ctx context.Context) ([]Automation, error)
	// Update applies a partial update: only non-zero fields change the row.
	// It recomputes NextRunAt when schedule-affecting fields change.
	Update(ctx context.Context, id string, patch AutomationPatch) (Automation, error)
	// Delete soft-deletes an automation.
	Delete(ctx context.Context, id string) error
	// NextDueAt returns ACTIVE, non-deleted automations with NextRunAt <= now.
	NextDueAt(ctx context.Context, now time.Time) ([]Automation, error)

	// UpdateNextRunAt advances a single automation's next_run_at (used by the
	// scheduler after a successful firing).
	UpdateNextRunAt(ctx context.Context, id string, next time.Time) error

	// SkipDue marks ACTIVE, non-deleted, overdue automations as skipped (the R15
	// daemon-restart reconcile path): it records a skipped run for each and returns
	// a count. It does not fire them.
	SkipDue(ctx context.Context, now time.Time) (int, error)

	// RecordRun inserts one run record.
	RecordRun(ctx context.Context, r Run) error
	// ListRuns returns runs for an automation, newest first.
	ListRuns(ctx context.Context, automationID string, limit int) ([]Run, error)
	// MarkRunRead marks a run as read.
	MarkRunRead(ctx context.Context, runID string) error

	// UpdateRuntimeState upserts the hot mutable state for an automation.
	UpdateRuntimeState(ctx context.Context, s RuntimeState) error
	// RuntimeState returns the hot state for an automation (zero value if none).
	RuntimeState(ctx context.Context, automationID string) (RuntimeState, error)

	Close() error
}

// AutomationPatch is the partial-update shape for Update. Zero fields are left
// unchanged, matching workbuddy's "only pass what you change" semantics.
type AutomationPatch struct {
	Name                 *string
	Prompt               *string
	Status               *Status
	ScheduleType         *ScheduleType
	RRule                *string
	ScheduledAt          *time.Time
	Timezone             *string
	ModeExec             *RunMode
	SessionID            *string
	CWDs                 *[]string
	ModelID              *string
	Skills               *[]string
	Connectors           *[]string
	PermissionMode       *string
	ValidFrom            *time.Time
	ValidUntil           *time.Time
	CreatedFromWorkspace *string
	LastRunAt            *time.Time
	LastStatus           *string
	RetryCount           *int
}

// computeNextRunAt returns the next firing time for an automation, in its
// creation timezone. For once automations it returns ScheduledAt. For recurring
// it parses the RRULE and computes the next occurrence strictly after `after`.
// The caller passes `after` so the scheduler can step from the last firing.
func computeNextRunAt(a Automation, after time.Time) (time.Time, error) {
	loc, err := loadLocation(a.Timezone)
	if err != nil {
		return time.Time{}, err
	}
	if a.ScheduleType == ScheduleOnce {
		if a.ScheduledAt.IsZero() {
			return time.Time{}, fmt.Errorf("once automation requires scheduled_at")
		}
		return a.ScheduledAt.In(loc), nil
	}
	r, err := parseRRule(a.RRule)
	if err != nil {
		return time.Time{}, err
	}
	return r.Next(after, loc)
}

// NormalizePermissionMode canonicalizes a permission_mode wire value onto the
// approval-tier vocabulary shared with the runtime: "ask" | "auto" | "full",
// with "full_access" accepted as the legacy alias of "full". "" is preserved —
// it means "inherit the workspace tier" (the daemon applies no override, so the
// firing runs at whatever tier the workspace's settings.local.json declares).
// ok=false for unknown values, which callers must reject at creation/update.
func NormalizePermissionMode(mode string) (string, bool) {
	switch mode {
	case "", "ask", "auto", "full":
		return mode, true
	case "full_access":
		return "full", true
	default:
		return "", false
	}
}

func loadLocation(tz string) (*time.Location, error) {
	if tz == "" {
		return time.UTC, nil
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return nil, fmt.Errorf("unknown timezone %q: %w", tz, err)
	}
	return loc, nil
}

var _ Store = (*sqlStore)(nil)
