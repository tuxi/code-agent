package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"unicode/utf8"

	"code-agent/internal/session"
)

func (q *UserQueue) TakeBackUserQueueMessage(ctx context.Context, sid, scope, rid, takeBackID string, revision int64) (session.QueuedMessageReceipt, error) {
	var receipt session.QueuedMessageReceipt
	if scope == "" || revision < 0 || rid == "" || len(rid) > 256 ||
		strings.TrimSpace(takeBackID) == "" || len(takeBackID) > 256 ||
		!utf8.ValidString(takeBackID) || strings.ContainsRune(takeBackID, 0) {
		return receipt, session.ErrInvalidQueuedMessage
	}
	tx, err := q.deliveryTransaction(ctx, sid)
	if err != nil {
		return receipt, err
	}
	defer tx.Rollback()
	receipt, err = queryQueuedUserMessage(ctx, tx, sid, rid)
	if err != nil {
		return receipt, err
	}
	if receipt.Input.Context.AuthorizationScope != scope {
		return session.QueuedMessageReceipt{}, session.ErrUserQueueUnavailable
	}
	if receipt.TakeBackID == takeBackID {
		return receipt, tx.Commit()
	}
	var original string
	err = tx.QueryRowContext(ctx, `SELECT request_id FROM user_queue_take_backs WHERE session_id=? AND take_back_id=?`, sid, takeBackID).Scan(&original)
	if err == nil {
		return session.QueuedMessageReceipt{}, session.ErrQueuedMessageConflict
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return session.QueuedMessageReceipt{}, err
	}
	var actualRevision int64
	var archived string
	err = tx.QueryRowContext(ctx, `SELECT q.revision,COALESCE(s.archived_at,'') FROM user_queue_control q JOIN sessions s ON s.id=q.session_id WHERE q.session_id=?`, sid).Scan(&actualRevision, &archived)
	if err != nil {
		return session.QueuedMessageReceipt{}, err
	}
	if archived != "" || receipt.State != "queued" {
		return session.QueuedMessageReceipt{}, session.ErrUserQueueUnavailable
	}
	if revision != actualRevision {
		return session.QueuedMessageReceipt{}, session.ErrUserQueueRevision
	}
	// The write lock covers both the state change and its receipt, so the
	// executor cannot claim the item between removing it and recording ownership.
	if err = q.removeUnstartedQueueMessage(ctx, tx, sid, scope, rid); err != nil {
		return session.QueuedMessageReceipt{}, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO user_queue_take_backs(session_id,request_id,take_back_id) VALUES(?,?,?)`, sid, rid, takeBackID); err != nil {
		return session.QueuedMessageReceipt{}, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE user_queue_control SET revision=revision+1 WHERE session_id=?`, sid); err != nil {
		return session.QueuedMessageReceipt{}, err
	}
	receipt.State = "removed"
	receipt.TakeBackID = takeBackID
	return receipt, tx.Commit()
}

var _ session.UserQueueTakeBackStore = (*UserQueue)(nil)
