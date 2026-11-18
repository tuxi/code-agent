package session

import (
	"context"
	"errors"
	"time"
)

var (
	ErrInvalidQueuedMessage  = errors.New("invalid queued message")
	ErrQueuedMessageConflict = errors.New("queued message request identity conflict")
	ErrUserQueueUnavailable  = errors.New("user queue session unavailable")
	ErrUserQueueRevision     = errors.New("user queue revision changed")
)

// QueueExecutionContext is a frozen execution selection, not an authorization
// grant. Admission must resolve it from authenticated server state; consumers
// must revalidate it before starting. It must never contain credentials.
type QueueExecutionContext struct {
	WireModel          string `json:"wire_model"`
	ResolvedModel      string `json:"resolved_model"`
	ReasoningEffort    string `json:"reasoning_effort,omitempty"`
	WorkspaceRoot      string `json:"workspace_root"`
	WorkspaceRel       string `json:"workspace_rel"`
	WorkspaceExtID     string `json:"workspace_ext_id"`
	WorkspacePath      string `json:"workspace_path"`
	AuthorizationScope string `json:"authorization_scope"`
	ApprovalMode       string `json:"approval_mode"`
}

// QueuedMessage is text-only in this first storage batch. It is not a
// TurnInput: enqueueing must not reserve scheduler capacity or create a turn.
type QueuedMessage struct {
	SelectionEffort string                `json:"selection_effort,omitempty"`
	SessionID       string                `json:"session_id"`
	RequestID       string                `json:"request_id"`
	Text            string                `json:"text"`
	Context         QueueExecutionContext `json:"context"`
}

type QueuedMessageReceipt struct {
	Input     QueuedMessage `json:"input"`
	Sequence  int64         `json:"sequence"`
	Revision  int64         `json:"revision"` // revision at acceptance, unchanged on retry
	State     string        `json:"state"`
	CreatedAt time.Time     `json:"created_at"`
}

type UserQueueCursor struct {
	SessionID     string `json:"session_id"`
	Revision      int64  `json:"revision"`
	AfterSequence int64  `json:"after_sequence"`
}

type UserQueuePage struct {
	Revision    int64                  `json:"revision"`
	State       string                 `json:"state"`
	PauseReason string                 `json:"pause_reason,omitempty"`
	Items       []QueuedMessageReceipt `json:"items"`
	Next        *UserQueueCursor       `json:"next,omitempty"`
}

// UserQueueStore is optional. A storage implementation alone must not
// advertise a working queue until execution, recovery and transport are wired.
type UserQueueStore interface {
	EnqueueUserMessage(context.Context, QueuedMessage) (QueuedMessageReceipt, bool, error)
	QueuedUserMessage(context.Context, string, string) (QueuedMessageReceipt, error)
	ListUserQueue(context.Context, string, *UserQueueCursor, int) (UserQueuePage, error)
}

// UserQueueTurnEnd is submitted by the serialized executor only after its final
// session checkpoint succeeds. A waiting/approval boundary is not a completion.
type UserQueueTurnEnd struct {
	SessionID string
	RequestID string
	TurnID    string
	State     TurnInputState
	Event     EventRecord
}

type UserQueueClaim struct {
	Message            QueuedMessageReceipt `json:"message"`
	ExecutionRequestID string               `json:"execution_request_id"`
	TurnID             string               `json:"turn_id"`
	AfterRequestID     string               `json:"after_request_id"`
	EventSequence      int64                `json:"event_sequence"`
}

type UserQueueAdvance struct {
	Claim                 *UserQueueClaim `json:"claim,omitempty"`
	Revision              int64           `json:"revision"`
	State                 string          `json:"state"`
	PauseReason           string          `json:"pause_reason,omitempty"`
	BlockedReason         string          `json:"blocked_reason,omitempty"`
	TerminalEventSequence int64           `json:"terminal_event_sequence,omitempty"`
}

var (
	ErrUserQueueTerminalConflict = errors.New("user queue terminal outcome conflict")
	ErrUserQueueUnsafeBoundary   = errors.New("user queue completion has unfinished input")
)

// These are storage boundaries, not execution authorization. Reconciliation
// returns a committed claim; it never starts a runner or grants credentials.
type UserQueueDeliveryStore interface {
	FinishUserQueueTurn(context.Context, UserQueueTurnEnd) (UserQueueAdvance, error)
	ReconcileUserQueue(context.Context, string) (UserQueueAdvance, error)
}

var ErrUserQueueContextChanged = errors.New("queued execution context changed")

// Current must come from fresh authenticated server-side resolution, not from
// the queue payload. Reservation is admission only; it is not a started receipt.
type UserQueueAdmissionStore interface {
	ReserveUserQueueClaim(ctx context.Context, sessionID, requestID string, current QueueExecutionContext) (TurnInput, bool, int64, error)
}
