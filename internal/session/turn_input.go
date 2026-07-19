package session

import (
	"context"
	"time"

	"code-agent/internal/model"
)

type TurnInputState string

const (
	TurnInputAccepted  TurnInputState = "accepted"
	TurnInputQueued    TurnInputState = "queued"
	TurnInputRunning   TurnInputState = "running"
	TurnInputCompleted TurnInputState = "completed"
	TurnInputFailed    TurnInputState = "failed"
	TurnInputCancelled TurnInputState = "cancelled"
)

// TurnInput is the durable v1.5 submission inbox. WireModel is payload identity;
// ResolvedModel is frozen execution identity and deliberately excluded from the
// payload hash.
type TurnInput struct {
	SessionID     string
	RequestID     string
	TurnID        string
	PayloadHash   string
	Text          string
	WireModel     string
	ResolvedModel string
	Assets        []model.GatewayAssetRef
	State         TurnInputState
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// TurnInputStore is optional so existing external stores remain source
// compatible. A Runtime may advertise image_input only when this durable port is
// available.
type TurnInputStore interface {
	ReserveTurnInput(ctx context.Context, input TurnInput, accepted EventRecord) (stored TurnInput, created bool, eventSeq int64, err error)
	StartTurnInput(ctx context.Context, input TurnInput, sess *Session) error
	SetTurnInputState(ctx context.Context, sessionID, requestID string, state TurnInputState) error
	TurnInput(ctx context.Context, sessionID, requestID string) (TurnInput, error)
	RecoverableTurnInputs(ctx context.Context) ([]TurnInput, error)
}
