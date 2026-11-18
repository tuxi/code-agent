package agent

import (
	"context"
	"strings"

	"code-agent/internal/session"
)

// Runtime input delivery is cross-cutting: the loop knows neither producer
// names nor process registries. Persistence owns the delivery/finish race.
func (r *Runner) runtimeInputBoundary(ctx context.Context, sess *session.Session, deliver, finalize bool) (int, error) {
	if r.Delivery != nil {
		if r.deliveryClosed {
			return 0, nil
		}
		result, err := r.Delivery.CheckpointTurnDelivery(ctx, sess, session.TurnDeliveryBoundary{TurnID: r.emitTurnID, Receipts: runtimeInputReceipts(sess), Deliver: deliver, Finalize: finalize})
		if err != nil {
			return 0, err
		}
		r.deliveryClosed = result.Closed
		if len(result.Applications) > 0 && r.OnSupplementsApplied != nil {
			r.OnSupplementsApplied(result.Applications)
		}
		if len(result.RuntimeIDs) > 0 {
			r.emit(Event{Kind: EventRuntimeInputDelivered, Text: strings.Join(result.RuntimeIDs, ",")})
		}
		return len(result.Applications) + len(result.RuntimeIDs), nil
	}
	if r.RuntimeInputs == nil {
		return 0, nil
	}
	ids, err := r.RuntimeInputs.CheckpointRuntimeInputs(ctx, sess, session.RuntimeInputBoundary{
		TurnID: r.emitTurnID, Receipts: runtimeInputReceipts(sess), Deliver: deliver, Finalize: finalize,
	})
	if err != nil {
		return 0, err
	}
	if len(ids) > 0 {
		r.emit(Event{Kind: EventRuntimeInputDelivered, Text: strings.Join(ids, ",")})
	}
	return len(ids), nil
}

func runtimeInputReceipts(sess *session.Session) []string {
	var ids []string
	for _, message := range sess.Messages {
		ids = append(ids, message.RuntimeInputReceipts...)
	}
	return ids
}
