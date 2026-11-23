package session

import (
	"context"
	"time"

	"code-agent/internal/collab"
	"code-agent/internal/model"
)

type TurnInputState string

const (
	TurnInputAccepted          TurnInputState = "accepted"
	TurnInputQueued            TurnInputState = "queued"
	TurnInputRunning           TurnInputState = "running"
	TurnInputCompleted         TurnInputState = "completed"
	TurnInputFailed            TurnInputState = "failed"
	TurnInputCancelled         TurnInputState = "cancelled"
	TurnInputWaitingBackground TurnInputState = "waiting_background"
)

// CrossSessionProvenance is durable routing metadata for Agent-originated
// input. It is deliberately kept out of the prompt text so the receiving
// model cannot confuse transport identity with user instructions.
type CrossSessionProvenance struct {
	SenderSessionID string
	SenderTurnID    string
	MessageID       string
	CorrelationID   string
	Intent          string
}

// FollowUpReply links a user submission back to a specific option on the
// previous turn's follow-up card. It is durable provenance only; the model sees
// the selected message text and the previous tool call in normal history.
type FollowUpReply struct {
	TurnID     string `json:"turn_id"`
	FollowUpID string `json:"follow_up_id"`
	OptionID   string `json:"option_id,omitempty"`
}

// TurnInput is the durable v1.5 submission inbox. WireModel is payload identity;
// ResolvedModel is frozen execution identity and deliberately excluded from the
// payload hash.
type TurnInput struct {
	SessionID          string
	RequestID          string
	TurnID             string
	PayloadHash        string
	Text               string
	WireModel          string
	ResolvedModel      string
	Assets             []model.GatewayAssetRef
	LocalAssets        []model.LocalAssetRef
	Provenance         *CrossSessionProvenance
	ReplyTo            *FollowUpReply
	ManagedTask        *collab.ManagedTurnRef
	ValidationContract *ValidationContract // authoritative content lives in the accepted event
	AcceptedSeq        int64
	State              TurnInputState
	CreatedAt          time.Time
	UpdatedAt          time.Time
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
