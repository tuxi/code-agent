package sqlite

import (
	"context"
	"database/sql"
	"errors"

	"code-agent/internal/session"
)

func (q *UserQueue) ControlUserQueue(ctx context.Context, sid, scope string, change session.UserQueueControl) error {
	if scope == "" || change.Revision == nil || *change.Revision < 0 ||
		(change.Action != "resume" && change.Action != "remove") ||
		(change.Action == "remove") != (change.RequestID != "") {
		return session.ErrInvalidQueuedMessage
	}
	tx, err := q.deliveryTransaction(ctx, sid)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var revision int64
	var state, archived string
	if err = tx.QueryRowContext(ctx, `SELECT q.revision,q.state,COALESCE(s.archived_at,'') FROM user_queue_control q JOIN sessions s ON s.id=q.session_id WHERE q.session_id=?`, sid).Scan(&revision, &state, &archived); err != nil {
		return err
	}
	if archived != "" {
		return session.ErrUserQueueUnavailable
	}
	if revision != *change.Revision {
		return session.ErrUserQueueRevision
	}

	if change.Action == "remove" {
		if err = q.removeUnstartedQueueMessage(ctx, tx, sid, scope, change.RequestID); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE user_queue_control SET revision=revision+1 WHERE session_id=?`, sid)
	} else {
		if state != "paused" {
			return session.ErrUserQueueUnavailable
		}
		if err = q.resumeUserQueueTx(ctx, tx, sid, scope); err != nil {
			return err
		}
	}
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (q *UserQueue) removeUnstartedQueueMessage(ctx context.Context, tx *sql.Tx, sid, scope, rid string) error {
	var hash string
	item, err := scanQueuedMessage(tx.QueryRowContext(ctx, `SELECT payload,sequence,accepted_revision,state,created_at,payload_hash FROM user_queue_messages WHERE session_id=? AND request_id=?`, sid, rid), &hash)
	if err != nil {
		return err
	}
	if item.Input.Context.AuthorizationScope != scope || (item.State != "queued" && item.State != "claimed") {
		return session.ErrUserQueueUnavailable
	}
	// A live reservation belongs to the executor. A terminal reservation
	// is removable only if it never started or persisted a user message.
	if item.State == "claimed" {
		result, err := tx.ExecContext(ctx, `UPDATE user_queue_claims SET active=0 WHERE session_id=? AND request_id=? AND active=1
   AND NOT EXISTS(SELECT 1 FROM turn_inputs i WHERE i.session_id=user_queue_claims.session_id AND i.request_id=user_queue_claims.execution_request_id AND i.state NOT IN ('failed','cancelled'))
   AND NOT EXISTS(SELECT 1 FROM session_events e WHERE e.session_id=user_queue_claims.session_id AND e.turn_id=user_queue_claims.turn_id AND e.kind='turn_started')
   AND NOT EXISTS(SELECT 1 FROM messages m WHERE m.session_id=user_queue_claims.session_id AND m.origin_turn_id=user_queue_claims.turn_id)`, sid, rid)
		if err != nil {
			return err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if n != 1 {
			return session.ErrUserQueueUnavailable
		}
	}
	// Preserve the request identity so retrying enqueue cannot resurrect it.
	_, err = tx.ExecContext(ctx, `UPDATE user_queue_messages SET state='removed' WHERE session_id=? AND request_id=?`, sid, rid)
	return err
}

func (q *UserQueue) resumeUserQueueTx(ctx context.Context, tx *sql.Tx, sid, scope string) error {
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM user_queue_messages WHERE session_id=? AND state IN ('queued','claimed') AND json_extract(payload,'$.context.authorization_scope') IS NOT ?`, sid, scope).Scan(&count); err != nil {
		return err
	}
	if count != 0 {
		return session.ErrUserQueueUnavailable
	}
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM user_queue_claims c JOIN turn_inputs i ON i.session_id=c.session_id AND i.request_id=c.execution_request_id
     WHERE c.session_id=? AND c.active=1 AND i.state IN ('failed','cancelled')`, sid).Scan(&count); err != nil {
		return err
	}
	if count != 0 {
		return session.ErrUserQueueUnavailable
	}
	// Interrupted runs may have side effects. Never clear that pause from here.
	var reason string
	if err := tx.QueryRowContext(ctx, `SELECT pause_reason FROM user_queue_control WHERE session_id=?`, sid).Scan(&reason); err != nil {
		return err
	}
	if reason == "execution_interrupted" {
		return session.ErrUserQueueUnavailable
	}

	var seq int64
	// Acknowledge only the latest terminal failure/cancellation that was
	// already reconciled. A later failure must still pause the queue.
	err := tx.QueryRowContext(ctx, `SELECT f.event_seq FROM user_queue_finishes f JOIN turn_inputs i ON i.session_id=f.session_id AND i.request_id=f.request_id
  WHERE f.session_id=? AND f.state IN ('failed','cancelled') AND f.state=i.state
  AND i.request_id=(SELECT request_id FROM turn_inputs WHERE session_id=? ORDER BY accepted_seq DESC,created_at DESC,rowid DESC LIMIT 1)`, sid, sid).Scan(&seq)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if seq != 0 {
		if _, err = tx.ExecContext(ctx, `INSERT INTO user_queue_resumes(session_id,terminal_event_seq) VALUES(?,?) ON CONFLICT(session_id) DO UPDATE SET terminal_event_seq=excluded.terminal_event_seq`, sid, seq); err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx, `UPDATE user_queue_control SET state='enabled',pause_reason='',revision=revision+1 WHERE session_id=?`, sid)
	return err
}

var _ session.UserQueueControlStore = (*UserQueue)(nil)
