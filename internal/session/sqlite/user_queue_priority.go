package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"code-agent/internal/session"
)

func (q *UserQueue) PrioritizeUserQueueMessage(ctx context.Context, sid, scope string, change session.UserQueueControl) error {
	if scope == "" || change.Action != "prioritize" || change.Revision == nil || *change.Revision < 0 ||
		strings.TrimSpace(change.RequestID) == "" || len(change.RequestID) > 256 ||
		strings.TrimSpace(change.OperationID) == "" || len(change.OperationID) > 256 || strings.ContainsRune(change.OperationID, 0) {
		return session.ErrInvalidQueuedMessage
	}
	tx, err := q.deliveryTransaction(ctx, sid)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	item, err := queryQueuedUserMessage(ctx, tx, sid, change.RequestID)
	if err != nil {
		return err
	}
	if item.Input.Context.AuthorizationScope != scope {
		return session.ErrUserQueueUnavailable
	}

	var requestID string
	var appliedRevision int64
	err = tx.QueryRowContext(ctx, `SELECT request_id,applied_revision FROM user_queue_priority_operations WHERE session_id=? AND operation_id=?`, sid, change.OperationID).Scan(&requestID, &appliedRevision)
	if err == nil {
		if requestID != change.RequestID || appliedRevision-1 != *change.Revision {
			return session.ErrQueuedMessageConflict
		}
		return tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}

	var revision int64
	var state, archived string
	err = tx.QueryRowContext(ctx, `SELECT q.revision,q.state,COALESCE(s.archived_at,'') FROM user_queue_control q
 JOIN sessions s ON s.id=q.session_id WHERE q.session_id=?`, sid).Scan(&revision, &state, &archived)
	if err != nil {
		return err
	}
	if archived != "" || state != "enabled" || item.State != "queued" {
		return session.ErrUserQueueUnavailable
	}
	if revision != *change.Revision {
		return session.ErrUserQueueRevision
	}

	// The queue revision is a monotonic order key. Receipt replays never
	// overwrite an order chosen by a later operation.
	if _, err = tx.ExecContext(ctx, `INSERT INTO user_queue_priority_operations(session_id,operation_id,request_id,applied_revision)
 VALUES(?,?,?,?)`, sid, change.OperationID, change.RequestID, revision+1); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE user_queue_control SET revision=revision+1 WHERE session_id=?`, sid); err != nil {
		return err
	}
	return tx.Commit()
}

var _ session.UserQueuePriorityStore = (*UserQueue)(nil)
