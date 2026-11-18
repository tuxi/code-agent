package session

import (
	"context"
	"errors"
)

var ErrTurnDeliveryPending = errors.New("turn still has pending delivery")

type TurnDeliveryBoundary struct {
	TurnID   string
	Receipts []string
	Deliver  bool
	Finalize bool
}
type TurnDeliveryResult struct {
	RuntimeIDs   []string
	Applications []SupplementApplication
	Closed       bool
}

// TurnDeliveryStore owns the shared close decision; separate stores cannot
// safely emulate this by finalizing one and then the other.
type TurnDeliveryStore interface {
	CheckpointTurnDelivery(context.Context, *Session, TurnDeliveryBoundary) (TurnDeliveryResult, error)
}
