package sqlite

import (
	"code-agent/internal/session"
	"context"
)

func (s *SupplementInbox) CheckpointTurnDelivery(ctx context.Context, sess *session.Session, b session.TurnDeliveryBoundary) (session.TurnDeliveryResult, error) {
	result := session.TurnDeliveryResult{}
	if sess == nil || sess.ID == "" || b.TurnID == "" {
		return result, session.ErrInvalidSupplement
	}
	tx, err := s.store.db.BeginTx(ctx, nil)
	if err != nil {
		return result, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `UPDATE user_supplement_boundaries SET sealed=sealed WHERE session_id=? AND turn_id=?`, sess.ID, b.TurnID); err != nil {
		return result, err
	}
	var open int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM user_supplement_boundaries b JOIN turn_inputs i ON i.session_id=b.session_id AND i.turn_id=b.turn_id WHERE b.session_id=? AND b.turn_id=? AND b.sealed=0 AND i.state='running'`, sess.ID, b.TurnID).Scan(&open); err != nil {
		return result, err
	}
	if open == 0 {
		return result, session.ErrSupplementTargetClosed
	}
	candidate := *sess
	if b.Deliver {
		// Reuse the same validation and balanced-history guard as standalone apply.
		pending := map[string]bool{}
		for _, m := range sess.Messages {
			if m.Role == "assistant" {
				for _, c := range m.ToolCalls {
					pending[c.ID] = true
				}
			}
			if m.Role == "tool" {
				delete(pending, m.ToolCallID)
			}
		}
		if len(pending) > 0 {
			return result, session.ErrSupplementUnsafeBoundary
		}
	}
	result.Applications, err = s.applySupplementsTx(ctx, tx, &candidate, b.TurnID, false, b.Deliver)
	if err != nil {
		return session.TurnDeliveryResult{}, err
	}

	result.RuntimeIDs, err = s.store.checkpointRuntimeInputsTx(ctx, tx, &candidate, session.RuntimeInputBoundary{TurnID: b.TurnID, Receipts: b.Receipts, Deliver: b.Deliver})
	if err != nil {
		return session.TurnDeliveryResult{}, err
	}
	if b.Finalize && len(result.Applications)+len(result.RuntimeIDs) == 0 {
		var count int
		if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM user_supplements WHERE session_id=? AND turn_id=? AND state='pending_apply'`, sess.ID, b.TurnID).Scan(&count); err != nil {
			return result, err
		}
		if count > 0 {
			return session.TurnDeliveryResult{}, session.ErrTurnDeliveryPending
		}
		if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM runtime_inputs WHERE session_id=? AND state='pending' AND kind!=''`, sess.ID).Scan(&count); err != nil {
			return result, err
		}
		if count > 0 {
			return session.TurnDeliveryResult{}, session.ErrTurnDeliveryPending
		}
		if _, err = tx.ExecContext(ctx, `UPDATE user_supplement_boundaries SET sealed=1 WHERE session_id=? AND turn_id=? AND sealed=0`, sess.ID, b.TurnID); err != nil {
			return result, err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE runtime_input_boundaries SET sealed=1 WHERE session_id=? AND turn_id=?`, sess.ID, b.TurnID); err != nil {
			return result, err
		}
		result.Closed = true
	}
	if err = tx.Commit(); err != nil {
		return session.TurnDeliveryResult{}, err
	}
	*sess = candidate
	if s.store.OnSave != nil && (len(result.Applications)+len(result.RuntimeIDs)+len(b.Receipts) > 0) {
		s.store.OnSave(sess, s.store.path)
	}
	return result, nil
}

var _ session.TurnDeliveryStore = (*SupplementInbox)(nil)

// ResumeTurnDeliveryBoundary never revives a stopped continuation or a sealed
// delivery boundary. Cancellation and this update share the SQLite writer.
func (s *SupplementInbox) ResumeTurnDeliveryBoundary(ctx context.Context, sid, tid string) error {
	result, err := s.store.db.ExecContext(ctx, `UPDATE turn_inputs SET state='running' WHERE session_id=? AND turn_id=? AND state IN ('running','waiting_background')
 AND EXISTS(SELECT 1 FROM runtime_continuations c WHERE c.session_id=? AND c.turn_id=? AND c.state='active')
 AND EXISTS(SELECT 1 FROM user_supplement_boundaries b WHERE b.session_id=? AND b.turn_id=? AND b.sealed=0)
 AND EXISTS(SELECT 1 FROM sessions s WHERE s.id=? AND COALESCE(s.archived_at,'')='')`, sid, tid, sid, tid, sid, tid, sid)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return session.ErrSupplementTargetClosed
	}
	return nil
}
