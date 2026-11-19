package session

import (
	"context"
	"errors"
	"time"

	"code-agent/internal/model"
)

var (
	ErrSupplementTargetClosed   = errors.New("supplement target is not accepting input")
	ErrSupplementConflict       = errors.New("supplement request identity conflict")
	ErrSupplementPending        = errors.New("supplement target still has unapplied input")
	ErrSupplementStaleContext   = errors.New("supplement context lacks committed application receipts")
	ErrSupplementUnsafeBoundary = errors.New("supplement boundary has unresolved tool calls")
	ErrInvalidSupplement        = errors.New("invalid supplement")
)

// Supplement inherits its target turn's execution settings; it cannot grant
// permissions or select another model or workspace.
type Supplement struct {
	SessionID      string                  `json:"session_id"`
	RequestID      string                  `json:"request_id"`
	ExpectedTurnID string                  `json:"expected_turn_id"`
	Text           string                  `json:"text"`
	Assets         []model.GatewayAssetRef `json:"assets,omitempty"`
	LocalAssets    []model.LocalAssetRef   `json:"local_assets,omitempty"`
}

type SupplementReceipt struct {
	Input    Supplement `json:"input"`
	Sequence int64      `json:"sequence"`
	State    string     `json:"state"`
}

// SupplementStore is an optional delivery port, separate from background
// RuntimeInput. Opening a boundary is an executor responsibility, not admission
// authorization for a transport caller.
type SupplementApplication struct {
	AppliedAt     time.Time `json:"applied_at"`
	RequestID     string    `json:"request_id"`
	TurnID        string    `json:"turn_id"`
	Sequence      int64     `json:"sequence"`
	Text          string    `json:"text,omitempty"`
	EventSequence int64     `json:"-"`
}

type SupplementStore interface {
	ApplySupplements(context.Context, *Session, string, bool) ([]SupplementApplication, error)
	OpenSupplementBoundary(context.Context, string, string) error
	AcceptSupplement(context.Context, Supplement) (SupplementReceipt, bool, error)
	Supplement(context.Context, string, string) (SupplementReceipt, error)
	PendingSupplements(context.Context, string, string) ([]SupplementReceipt, error)
	SealSupplementBoundary(context.Context, string, string) error
}
