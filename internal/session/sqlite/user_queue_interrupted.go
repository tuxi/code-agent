package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"code-agent/internal/session"
)

func (q *UserQueue) requestInterruptedQueueTx(ctx context.Context, tx *sql.Tx, sid, scope, rid string) error {
	var tid, body string
	err := tx.QueryRowContext(ctx, `SELECT c.turn_id,m.payload FROM user_queue_claims c
 JOIN user_queue_messages m ON m.session_id=c.session_id AND m.request_id=c.request_id
 JOIN turn_inputs i ON i.session_id=c.session_id AND i.request_id=c.execution_request_id
 JOIN user_queue_control q ON q.session_id=c.session_id
 JOIN sessions s ON s.id=c.session_id
 WHERE c.session_id=? AND c.request_id=? AND c.active=1 AND m.state='started' AND i.state='running'
 AND q.state='paused' AND q.pause_reason IN ('execution_interrupted','resume_validation_failed')
 AND json_extract(s.metadata,'$.turn_status')='paused'`, sid, rid).Scan(&tid, &body)
	if err != nil {
		return err
	}
	var background int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM runtime_continuations WHERE session_id=? AND checkpoint!='' AND state NOT IN ('finished','stopped','superseded')`, sid).Scan(&background); err != nil {
		return err
	}
	if background != 0 {
		return session.ErrUserQueueUnavailable
	}
	var message session.QueuedMessage
	if err = json.Unmarshal([]byte(body), &message); err != nil {
		return err
	}
	if message.Context.AuthorizationScope != scope {
		return session.ErrUserQueueUnavailable
	}
	if _, err = tx.ExecContext(ctx, `UPDATE user_queue_control SET pause_reason='resume_requested',revision=revision+1 WHERE session_id=?`, sid); err != nil {
		return err
	}
	now := time.Now().UTC()
	payload, _ := json.Marshal(map[string]any{"Kind": "user_queue_resume_requested", "SessionID": sid, "TurnID": tid, "RequestID": rid, "At": now})
	_, err = tx.ExecContext(ctx, `INSERT INTO session_events(session_id,turn_id,kind,at,payload) VALUES(?,?,'user_queue_resume_requested',?,?)`, sid, tid, formatTime(now), string(payload))
	return err
}

func (q *UserQueue) PendingInterruptedUserQueue(ctx context.Context, sid string) (session.TurnInput, session.QueueExecutionContext, error) {
	var rid, body string
	err := q.store.db.QueryRowContext(ctx, `SELECT c.execution_request_id,m.payload FROM user_queue_claims c
 JOIN user_queue_messages m ON m.session_id=c.session_id AND m.request_id=c.request_id
 JOIN turn_inputs i ON i.session_id=c.session_id AND i.request_id=c.execution_request_id
 JOIN user_queue_control q ON q.session_id=c.session_id JOIN sessions s ON s.id=c.session_id
 WHERE c.session_id=? AND c.active=1 AND m.state='started' AND i.state='running'
 AND q.state='paused' AND q.pause_reason='resume_requested' AND COALESCE(s.archived_at,'')=''
 AND json_extract(s.metadata,'$.turn_status')='paused'`, sid).Scan(&rid, &body)
	if err != nil {
		return session.TurnInput{}, session.QueueExecutionContext{}, err
	}
	var message session.QueuedMessage
	if err = json.Unmarshal([]byte(body), &message); err != nil {
		return session.TurnInput{}, session.QueueExecutionContext{}, err
	}
	input, err := q.store.TurnInput(ctx, sid, rid)
	return input, message.Context, err
}

func (q *UserQueue) StartInterruptedUserQueue(ctx context.Context, input session.TurnInput, sess *session.Session, current session.QueueExecutionContext) (int64, error) {
	tx, err := q.deliveryTransaction(ctx, input.SessionID)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var body, path, root, relative, externalID string
	err = tx.QueryRowContext(ctx, `SELECT m.payload,COALESCE(s.workspace_path,''),COALESCE(s.workspace_root,''),COALESCE(s.workspace_rel,''),COALESCE(s.workspace_ext_id,'') FROM user_queue_claims c
 JOIN user_queue_messages m ON m.session_id=c.session_id AND m.request_id=c.request_id
 JOIN turn_inputs i ON i.session_id=c.session_id AND i.request_id=c.execution_request_id
 JOIN user_queue_control q ON q.session_id=c.session_id JOIN sessions s ON s.id=c.session_id
 WHERE c.session_id=? AND c.execution_request_id=? AND c.turn_id=? AND c.active=1
 AND m.state='started' AND i.state='running' AND q.state='paused' AND q.pause_reason='resume_requested'
 AND COALESCE(s.archived_at,'')='' AND json_extract(s.metadata,'$.turn_status')='paused'`, input.SessionID, input.RequestID, input.TurnID).Scan(&body, &path, &root, &relative, &externalID)
	if err != nil {
		return 0, err
	}
	var message session.QueuedMessage
	if err = json.Unmarshal([]byte(body), &message); err != nil {
		return 0, err
	}
	if path != current.WorkspacePath || root != current.WorkspaceRoot || relative != current.WorkspaceRel || externalID != current.WorkspaceExtID ||
		sess == nil || sess.ID != input.SessionID || sess.WorkspacePath != current.WorkspacePath ||
		sess.Workspace.Root != current.WorkspaceRoot || sess.Workspace.Rel != current.WorkspaceRel || sess.Workspace.ExtID != current.WorkspaceExtID ||
		input.Text != message.Text || input.WireModel != current.WireModel || input.ResolvedModel != current.ResolvedModel || message.Context != current {
		return 0, session.ErrUserQueueContextChanged
	}
	var latestRequest string
	if err = tx.QueryRowContext(ctx, `SELECT request_id FROM turn_inputs WHERE session_id=? ORDER BY accepted_seq DESC,created_at DESC,rowid DESC LIMIT 1`, input.SessionID).Scan(&latestRequest); err != nil {
		return 0, err
	}
	if latestRequest != input.RequestID {
		return 0, session.ErrUserQueueUnavailable
	}
	var competing int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM turn_inputs WHERE session_id=? AND request_id!=? AND state NOT IN ('completed','failed','cancelled')`, input.SessionID, input.RequestID).Scan(&competing); err != nil {
		return 0, err
	}
	if competing != 0 {
		return 0, session.ErrUserQueueUnavailable
	}
	// A parked background checkpoint has its own continuation protocol.
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM runtime_continuations WHERE session_id=? AND checkpoint!='' AND state NOT IN ('finished','stopped','superseded')`, input.SessionID).Scan(&competing); err != nil {
		return 0, err
	}
	if competing != 0 {
		return 0, session.ErrUserQueueUnavailable
	}
	copy := *sess
	copy.Metadata = make(map[string]any, len(sess.Metadata))
	for k, v := range sess.Metadata {
		copy.Metadata[k] = v
	}
	copy.ReasoningEffort = current.ReasoningEffort
	copy.SetTurnStatus(session.TurnStatusResuming)
	if err = q.store.saveSessionTx(ctx, tx, &copy); err != nil {
		return 0, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE user_queue_control SET state='enabled',pause_reason='',revision=revision+1 WHERE session_id=?`, input.SessionID); err != nil {
		return 0, err
	}
	now := time.Now().UTC()
	payload, _ := json.Marshal(map[string]any{"Kind": "user_queue_resumed", "SessionID": input.SessionID, "TurnID": input.TurnID, "RequestID": input.RequestID, "At": now})
	event, err := tx.ExecContext(ctx, `INSERT INTO session_events(session_id,turn_id,kind,at,payload) VALUES(?,?,'user_queue_resumed',?,?)`, input.SessionID, input.TurnID, formatTime(now), string(payload))
	if err != nil {
		return 0, err
	}
	seq, err := event.LastInsertId()
	if err != nil {
		return 0, err
	}
	if err = tx.Commit(); err != nil {
		return 0, err
	}
	sess.Metadata = copy.Metadata
	sess.ReasoningEffort = copy.ReasoningEffort
	return seq, nil
}

var _ session.InterruptedUserQueueStore = (*UserQueue)(nil)

func (q *UserQueue) PauseInterruptedUserQueue(ctx context.Context, sid string) error {
	tx, err := q.deliveryTransaction(ctx, sid)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var running bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM user_queue_claims c JOIN turn_inputs i ON i.session_id=c.session_id AND i.request_id=c.execution_request_id WHERE c.session_id=? AND c.active=1 AND i.state='running')`, sid).Scan(&running); err != nil {
		return err
	}
	if running {
		if _, err = tx.ExecContext(ctx, `UPDATE user_queue_control SET state='paused',pause_reason='resume_validation_failed',revision=revision+1 WHERE session_id=?`, sid); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE sessions SET metadata=json_set(COALESCE(NULLIF(metadata,''),'{}'),'$.turn_status','paused') WHERE id=? AND EXISTS(SELECT 1 FROM user_queue_claims c WHERE c.session_id=sessions.id AND c.active=1
 AND c.execution_request_id=(SELECT request_id FROM turn_inputs WHERE session_id=sessions.id ORDER BY accepted_seq DESC,created_at DESC,rowid DESC LIMIT 1))`, sid); err != nil {
			return err
		}
	}
	return tx.Commit()
}
