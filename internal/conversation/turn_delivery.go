package conversation

import (
	"code-agent/internal/agent"
	"code-agent/internal/session"
	"context"
	"errors"
)

type deliveryInbox interface {
	session.TurnDeliveryStore
	session.SupplementStore
}

// SetTurnDeliveryInbox is setup-only. Enabling transport admission and its
// capability remains separate from configuring the executor's boundary port.
func (e *TurnExecutor) SetTurnDeliveryInbox(inbox deliveryInbox) { e.deliveryInbox = inbox }

func (e *TurnExecutor) configureTurnDelivery(ctx context.Context, sid, tid string, runner TurnRunner, pub *sequencingEmitter) error {
	if e.deliveryInbox == nil {
		return nil
	}
	configurable, ok := runner.(interface {
		ConfigureTurnDelivery(session.TurnDeliveryStore, func([]session.SupplementApplication))
	})
	if !ok {
		return errors.New("runner does not support shared turn delivery")
	}
	if err := e.deliveryInbox.OpenSupplementBoundary(ctx, sid, tid); err != nil {
		return err
	}
	configurable.ConfigureTurnDelivery(e.deliveryInbox, func(receipts []session.SupplementApplication) {
		for _, receipt := range receipts {
			copy := receipt
			pub.EmitPersisted(agent.Event{Kind: agent.EventKind("user_supplement_applied"), SessionID: sid, TurnID: tid, RequestID: receipt.RequestID, Seq: receipt.EventSequence, At: receipt.AppliedAt, SupplementApplication: &copy})
		}
	})
	return nil
}

func (e *TurnExecutor) SupportsTextSupplements() bool { return e != nil && e.deliveryInbox != nil }

func (e *TurnExecutor) SubmitTextSupplement(ctx context.Context, in session.Supplement) (session.SupplementReceipt, bool, error) {
	if !e.SupportsTextSupplements() {
		return session.SupplementReceipt{}, false, ErrSupplementUnavailable
	}
	if len(in.Assets)+len(in.LocalAssets) > 0 {
		return session.SupplementReceipt{}, false, session.ErrInvalidSupplement
	}
	return e.deliveryInbox.AcceptSupplement(ctx, in)
}
func (e *TurnExecutor) QuerySupplement(ctx context.Context, sid, rid string) (session.SupplementReceipt, error) {
	if !e.SupportsTextSupplements() {
		return session.SupplementReceipt{}, ErrSupplementUnavailable
	}
	return e.deliveryInbox.Supplement(ctx, sid, rid)
}

var ErrSupplementUnavailable = errors.New("user supplement delivery is unavailable")
