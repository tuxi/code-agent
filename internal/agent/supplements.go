package agent

import (
	"context"

	"code-agent/internal/session"
)

func (r *Runner) supplementBoundary(ctx context.Context, sess *session.Session, finalize bool) (int, error) {
	if r.Delivery != nil || r.Supplements == nil {
		return 0, nil
	}
	applied, err := r.Supplements.ApplySupplements(ctx, sess, r.emitTurnID, finalize)
	if err != nil {
		return 0, err
	}
	// These receipts already have durable event identities. Do not re-emit them
	// through the ordinary emitter, which would persist them a second time.
	if len(applied) > 0 && r.OnSupplementsApplied != nil {
		r.OnSupplementsApplied(applied)
	}
	return len(applied), nil
}

func (r *Runner) ConfigureTurnDelivery(store session.TurnDeliveryStore, publish func([]session.SupplementApplication)) {
	r.Delivery = store
	r.OnSupplementsApplied = publish
}
