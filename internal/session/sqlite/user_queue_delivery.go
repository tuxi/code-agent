package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"code-agent/internal/session"
)

func ensureUserQueueDeliverySchema(db *sql.DB) error {
	_, err := db.Exec(`
 CREATE TABLE IF NOT EXISTS user_queue_finishes (
 session_id TEXT NOT NULL, request_id TEXT NOT NULL, turn_id TEXT NOT NULL,
 state TEXT NOT NULL, event_seq INTEGER NOT NULL,
 PRIMARY KEY(session_id,request_id), UNIQUE(session_id,turn_id));
 CREATE TABLE IF NOT EXISTS user_queue_claims (
 session_id TEXT NOT NULL, request_id TEXT NOT NULL, execution_request_id TEXT NOT NULL,
 turn_id TEXT NOT NULL, after_request_id TEXT NOT NULL, event_seq INTEGER NOT NULL,
 active INTEGER NOT NULL DEFAULT 1,
 PRIMARY KEY(session_id,request_id), UNIQUE(session_id,execution_request_id), UNIQUE(session_id,turn_id));
 CREATE TABLE IF NOT EXISTS user_queue_resumes (
 session_id TEXT PRIMARY KEY, terminal_event_seq INTEGER NOT NULL);
 CREATE TRIGGER IF NOT EXISTS user_queue_resume_session_delete AFTER DELETE ON sessions BEGIN
 DELETE FROM user_queue_resumes WHERE session_id=OLD.id;
 END;
 CREATE UNIQUE INDEX IF NOT EXISTS user_queue_one_claim ON user_queue_claims(session_id) WHERE active=1;
 CREATE TRIGGER IF NOT EXISTS user_queue_delivery_session_delete AFTER DELETE ON sessions BEGIN
 DELETE FROM user_queue_finishes WHERE session_id=OLD.id;
 DELETE FROM user_queue_claims WHERE session_id=OLD.id;
 END;`)
	return err
}

func queueTerminalKind(state session.TurnInputState) string {
	switch state {
	case session.TurnInputCompleted:
		return "turn_finished"
	case session.TurnInputFailed:
		return "turn_failed"
	case session.TurnInputCancelled:
		return "turn_cancelled"
	default:
		return ""
	}
}

func (q *UserQueue) deliveryTransaction(ctx context.Context, sid string) (*sql.Tx, error) {
	if sid == "" {
		return nil, session.ErrInvalidQueuedMessage
	}
	tx, err := q.store.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	result, err := tx.ExecContext(ctx, `UPDATE sessions SET updated_at=updated_at WHERE id=?`, sid)
	if err == nil {
		var n int64
		n, err = result.RowsAffected()
		if err == nil && n == 0 {
			err = session.ErrUserQueueUnavailable
		}
	}
	if err != nil {
		tx.Rollback()
		return nil, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO user_queue_control(session_id) VALUES(?) ON CONFLICT(session_id) DO NOTHING`, sid)
	if err != nil {
		tx.Rollback()
		return nil, err
	}
	return tx, nil
}

func (q *UserQueue) FinishUserQueueTurn(ctx context.Context, end session.UserQueueTurnEnd) (session.UserQueueAdvance, error) {
	empty := session.UserQueueAdvance{}
	kind := queueTerminalKind(end.State)
	var envelope struct{ Kind, SessionID, TurnID, RequestID string }
	if end.RequestID == "" || end.TurnID == "" || kind == "" || end.Event.SessionID != end.SessionID || end.Event.TurnID != end.TurnID || end.Event.Kind != kind || end.Event.At.IsZero() || json.Unmarshal(end.Event.Payload, &envelope) != nil || envelope.Kind != kind || envelope.SessionID != end.SessionID || envelope.TurnID != end.TurnID || (envelope.RequestID != "" && envelope.RequestID != end.RequestID) {
		return empty, session.ErrInvalidQueuedMessage
	}
	tx, err := q.deliveryTransaction(ctx, end.SessionID)
	if err != nil {
		return empty, err
	}
	defer tx.Rollback()
	result, err := q.finishUserQueueTurnTx(ctx, tx, end)
	if err != nil {
		return empty, err
	}
	if err = tx.Commit(); err != nil {
		return empty, err
	}
	return result, nil
}

func (q *UserQueue) finishUserQueueTurnTx(ctx context.Context, tx *sql.Tx, end session.UserQueueTurnEnd) (session.UserQueueAdvance, error) {
	empty := session.UserQueueAdvance{}
	kind := queueTerminalKind(end.State)
	var err error
	var state, tid string
	if err = tx.QueryRowContext(ctx, `SELECT state,turn_id FROM turn_inputs WHERE session_id=? AND request_id=?`, end.SessionID, end.RequestID).Scan(&state, &tid); err != nil {
		return empty, err
	}
	if tid != end.TurnID || (queueTerminalKind(session.TurnInputState(state)) != "" && state != string(end.State)) {
		return empty, session.ErrUserQueueTerminalConflict
	}
	switch session.TurnInputState(state) {
	case session.TurnInputAccepted, session.TurnInputQueued:
		if end.State == session.TurnInputCompleted {
			return empty, session.ErrUserQueueUnsafeBoundary
		}
	case session.TurnInputRunning, session.TurnInputCompleted, session.TurnInputFailed, session.TurnInputCancelled:
	case session.TurnInputWaitingBackground:
		if end.State == session.TurnInputCompleted {
			return empty, session.ErrUserQueueUnsafeBoundary
		}
	default:
		return empty, session.ErrUserQueueTerminalConflict
	}
	var recordedState string
	var terminalSeq int64
	err = tx.QueryRowContext(ctx, `SELECT state,event_seq FROM user_queue_finishes WHERE session_id=? AND request_id=?`, end.SessionID, end.RequestID).Scan(&recordedState, &terminalSeq)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return empty, err
	}
	if err == nil {
		if recordedState != string(end.State) {
			return empty, session.ErrUserQueueTerminalConflict
		}
	} else {
		if end.State == session.TurnInputCompleted {
			blocked, e := q.unfinishedQueueInput(ctx, tx, end.SessionID)
			if e != nil {
				return empty, e
			}
			if blocked != "" {
				return empty, session.ErrUserQueueUnsafeBoundary
			}
		}
		// Reuse an already-persisted terminal event during migration/recovery. Never
		// append a second terminal merely because its state projection lagged.
		var existingKind string
		err = tx.QueryRowContext(ctx, `SELECT id,kind FROM session_events WHERE session_id=? AND turn_id=? AND kind IN ('turn_finished','turn_failed','turn_cancelled') ORDER BY id DESC LIMIT 1`, end.SessionID, end.TurnID).Scan(&terminalSeq, &existingKind)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return empty, err
		}
		if err == nil && existingKind != kind {
			return empty, session.ErrUserQueueTerminalConflict
		}
		if errors.Is(err, sql.ErrNoRows) {
			result, e := tx.ExecContext(ctx, `INSERT INTO session_events(session_id,turn_id,kind,at,payload) VALUES(?,?,?,?,?)`, end.SessionID, end.TurnID, kind, formatTime(end.Event.At), string(end.Event.Payload))
			if e != nil {
				return empty, e
			}
			terminalSeq, e = result.LastInsertId()
			if e != nil {
				return empty, e
			}
		}
		if _, err = tx.ExecContext(ctx, `UPDATE turn_inputs SET state=?,updated_at=? WHERE session_id=? AND request_id=?`, string(end.State), formatTime(end.Event.At), end.SessionID, end.RequestID); err != nil {
			return empty, err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO user_queue_finishes(session_id,request_id,turn_id,state,event_seq) VALUES(?,?,?,?,?)`, end.SessionID, end.RequestID, end.TurnID, string(end.State), terminalSeq); err != nil {
			return empty, err
		}
		if end.State != session.TurnInputCompleted {
			if _, err = tx.ExecContext(ctx, `UPDATE user_queue_control SET state='paused',pause_reason=?,revision=revision+1 WHERE session_id=?`, string(end.State), end.SessionID); err != nil {
				return empty, err
			}
		}
		// Finishing a bound execution releases its claim without discarding the
		// queued message or its immutable execution mapping.
		var original string
		err = tx.QueryRowContext(ctx, `SELECT request_id FROM user_queue_claims WHERE session_id=? AND execution_request_id=? AND active=1`, end.SessionID, end.RequestID).Scan(&original)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return empty, err
		}

		if err == nil {
			// Cancellation can happen while a reserved input is still waiting
			// for a permit. It must not falsely mark that message as started.
			var started int
			if state == "running" || state == "waiting_background" || (state == "completed" && end.State == session.TurnInputCompleted) {
				started = 1
			}
			if started == 0 {
				if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM session_events WHERE session_id=? AND turn_id=? AND kind='turn_started'`, end.SessionID, end.TurnID).Scan(&started); err != nil {
					return empty, err
				}
			}
			if started == 0 {
				if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages WHERE session_id=? AND origin_turn_id=?`, end.SessionID, end.TurnID).Scan(&started); err != nil {
					return empty, err
				}
			}
			if started > 0 {
				if _, err = tx.ExecContext(ctx, `UPDATE user_queue_claims SET active=0 WHERE session_id=? AND request_id=?`, end.SessionID, original); err != nil {
					return empty, err
				}
				if _, err = tx.ExecContext(ctx, `UPDATE user_queue_messages SET state='started' WHERE session_id=? AND request_id=?`, end.SessionID, original); err != nil {
					return empty, err
				}
				if _, err = tx.ExecContext(ctx, `UPDATE user_queue_control SET revision=revision+1 WHERE session_id=?`, end.SessionID); err != nil {
					return empty, err
				}
			}
		}
	}
	result, err := q.advanceUserQueueTx(ctx, tx, end.SessionID)
	if err != nil {
		return empty, err
	}
	result.TerminalEventSequence = terminalSeq
	return result, nil
}

func (q *UserQueue) ReconcileUserQueue(ctx context.Context, sid string) (session.UserQueueAdvance, error) {
	tx, err := q.deliveryTransaction(ctx, sid)
	if err != nil {
		return session.UserQueueAdvance{}, err
	}
	defer tx.Rollback()
	if err = cancelClosedTurnSupplementsTx(ctx, tx, sid); err != nil {
		return session.UserQueueAdvance{}, err
	}
	// Import the latest already-durable outcome if a process exited between
	// the legacy terminal event and this queue boundary. Reuse that event and
	// the original execution mapping instead of replaying the finished task.
	// An interrupted started turn may have performed side effects. Only a
	// completed session checkpoint permits importing a successful event.
	if _, err = tx.ExecContext(ctx, `UPDATE user_queue_control SET state='paused',pause_reason='execution_interrupted',revision=revision+1
     WHERE session_id=? AND state='enabled' AND EXISTS(
       SELECT 1 FROM user_queue_claims c JOIN turn_inputs i ON i.session_id=c.session_id AND i.request_id=c.execution_request_id
       JOIN sessions s ON s.id=c.session_id WHERE c.session_id=? AND c.active=1 AND i.state='running'
       AND json_extract(CASE WHEN json_valid(s.metadata) THEN s.metadata ELSE '{}' END,'$.turn_status')='paused')`, sid, sid); err != nil {
		return session.UserQueueAdvance{}, err
	}
	var end session.UserQueueTurnEnd
	var kind, at, payload string
	err = tx.QueryRowContext(ctx, `SELECT i.request_id,i.turn_id,e.kind,e.at,e.payload
      FROM turn_inputs i JOIN session_events e ON e.id=(SELECT MAX(id) FROM session_events WHERE session_id=i.session_id AND turn_id=i.turn_id AND kind IN ('turn_finished','turn_failed','turn_cancelled'))
      WHERE (e.kind!='turn_finished' OR NOT EXISTS(SELECT 1 FROM user_queue_claims c WHERE c.session_id=i.session_id AND c.execution_request_id=i.request_id)
        OR EXISTS(SELECT 1 FROM sessions s WHERE s.id=i.session_id AND json_extract(CASE WHEN json_valid(s.metadata) THEN s.metadata ELSE '{}' END,'$.turn_status')='done'))
      AND i.session_id=? AND i.request_id=(SELECT request_id FROM turn_inputs WHERE session_id=? ORDER BY accepted_seq DESC,created_at DESC,rowid DESC LIMIT 1)
      AND NOT EXISTS(SELECT 1 FROM user_queue_finishes f WHERE f.session_id=i.session_id AND f.request_id=i.request_id)`, sid, sid).Scan(&end.RequestID, &end.TurnID, &kind, &at, &payload)
	var result session.UserQueueAdvance
	if err == nil {
		end.SessionID = sid
		switch kind {
		case "turn_finished":
			end.State = session.TurnInputCompleted
		case "turn_failed":
			end.State = session.TurnInputFailed
		case "turn_cancelled":
			end.State = session.TurnInputCancelled
		}
		end.Event = session.EventRecord{SessionID: sid, TurnID: end.TurnID, Kind: kind, At: parseTime(at), Payload: json.RawMessage(payload)}
		result, err = q.finishUserQueueTurnTx(ctx, tx, end)
	} else if errors.Is(err, sql.ErrNoRows) {
		result, err = q.advanceUserQueueTx(ctx, tx, sid)
	}
	if err != nil {
		return session.UserQueueAdvance{}, err
	}
	if err = tx.Commit(); err != nil {
		return session.UserQueueAdvance{}, err
	}
	return result, nil
}

// The caller holds the SQLite writer. Missing durable evidence means blocked,
// never permission to infer that an apparently idle session succeeded.
func (q *UserQueue) advanceUserQueueTx(ctx context.Context, tx *sql.Tx, sid string) (session.UserQueueAdvance, error) {
	var out session.UserQueueAdvance
	if err := tx.QueryRowContext(ctx, `SELECT revision,state,pause_reason FROM user_queue_control WHERE session_id=?`, sid).Scan(&out.Revision, &out.State, &out.PauseReason); err != nil {
		return out, err
	}
	var archived string
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(archived_at,'') FROM sessions WHERE id=?`, sid).Scan(&archived); err != nil {
		return out, err
	}
	if archived != "" {
		out.BlockedReason = "session_archived"
		return out, nil
	}
	if out.State != "enabled" {
		out.BlockedReason = "queue_paused"
		return out, nil
	}
	// A failure written through the old projection path must also stop recovery.
	var latestRequest, latestTurn, latestState string
	err := tx.QueryRowContext(ctx, `SELECT request_id,turn_id,state FROM turn_inputs WHERE session_id=? ORDER BY accepted_seq DESC,created_at DESC,rowid DESC LIMIT 1`, sid).Scan(&latestRequest, &latestTurn, &latestState)
	if errors.Is(err, sql.ErrNoRows) {
		out.BlockedReason = "no_durable_turn"
		return out, nil
	}
	if err != nil {
		return out, err
	}

	var terminalSeq int64
	var terminalKind string
	terminalErr := tx.QueryRowContext(ctx, `SELECT id,kind FROM session_events WHERE session_id=? AND turn_id=? AND kind IN ('turn_finished','turn_failed','turn_cancelled') ORDER BY id DESC LIMIT 1`, sid, latestTurn).Scan(&terminalSeq, &terminalKind)
	if terminalErr != nil && !errors.Is(terminalErr, sql.ErrNoRows) {
		return out, terminalErr
	}
	pauseReason := latestState
	if terminalKind == "turn_failed" {
		pauseReason = "failed"
	}
	if terminalKind == "turn_cancelled" {
		pauseReason = "cancelled"
	}
	var resumed bool
	if terminalErr == nil && (pauseReason == "failed" || pauseReason == "cancelled") {
		if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM user_queue_resumes WHERE session_id=? AND terminal_event_seq=?)`, sid, terminalSeq).Scan(&resumed); err != nil {
			return out, err
		}
		resumed = resumed && ((latestState == "failed" && terminalKind == "turn_failed") || (latestState == "cancelled" && terminalKind == "turn_cancelled"))
	}
	if !resumed && (pauseReason == "failed" || pauseReason == "cancelled") {
		if _, err = tx.ExecContext(ctx, `UPDATE user_queue_control SET state='paused',pause_reason=?,revision=revision+1 WHERE session_id=?`, pauseReason, sid); err != nil {
			return out, err
		}
		out.State, out.PauseReason, out.BlockedReason = "paused", pauseReason, "queue_paused"
		out.Revision++
		return out, nil
	}
	// Returning a committed claim is recovery, not a new dequeue. Check pause
	// first so a surviving claim cannot bypass a failure or cancellation.
	var claim session.UserQueueClaim
	var request string
	err = tx.QueryRowContext(ctx, `SELECT request_id,execution_request_id,turn_id,after_request_id,event_seq FROM user_queue_claims WHERE session_id=? AND active=1`, sid).Scan(&request, &claim.ExecutionRequestID, &claim.TurnID, &claim.AfterRequestID, &claim.EventSequence)
	if err == nil {
		var hash string
		claim.Message, err = scanQueuedMessage(tx.QueryRowContext(ctx, `SELECT payload,sequence,accepted_revision,state,created_at,payload_hash FROM user_queue_messages WHERE session_id=? AND request_id=?`, sid, request), &hash)
		if err != nil {
			return out, err
		}

		var competing int
		if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM turn_inputs WHERE session_id=? AND request_id!=? AND state NOT IN ('completed','failed','cancelled')`, sid, claim.ExecutionRequestID).Scan(&competing); err != nil {
			return out, err
		}
		if competing > 0 {
			out.BlockedReason = "turn_active"
			return out, nil
		}
		out.BlockedReason, err = q.unfinishedQueueInput(ctx, tx, sid)
		if err != nil || out.BlockedReason != "" {
			return out, err
		}
		out.Claim = &claim
		return out, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return out, err
	}
	var active int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM turn_inputs WHERE session_id=? AND state NOT IN ('completed','failed','cancelled')`, sid).Scan(&active); err != nil {
		return out, err
	}
	if active > 0 {
		out.BlockedReason = "turn_active"
		return out, nil
	}
	if latestState != "completed" && !resumed {
		out.BlockedReason = "turn_not_completed"
		return out, nil
	}

	if errors.Is(terminalErr, sql.ErrNoRows) {
		out.BlockedReason = "terminal_not_persisted"
		return out, nil
	}
	if terminalKind != "turn_finished" && !resumed {
		out.BlockedReason = "terminal_conflict"
		return out, nil
	}
	var newerAdmission int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM session_events WHERE session_id=? AND id>? AND kind IN ('turn_started','turn_accepted')`, sid, terminalSeq).Scan(&newerAdmission); err != nil {
		return out, err
	}
	if newerAdmission > 0 {
		out.BlockedReason = "newer_turn"
		return out, nil
	}
	out.BlockedReason, err = q.unfinishedQueueInput(ctx, tx, sid)
	if err != nil || out.BlockedReason != "" {
		return out, err
	}
	var hash string
	claim.Message, err = scanQueuedMessage(tx.QueryRowContext(ctx, `SELECT payload,sequence,accepted_revision,state,created_at,payload_hash FROM user_queue_messages WHERE session_id=? AND state='queued' ORDER BY sequence LIMIT 1`, sid), &hash)
	if errors.Is(err, sql.ErrNoRows) {
		out.BlockedReason = "queue_empty"
		return out, nil
	}
	if err != nil {
		return out, err
	}
	// The separate request namespace cannot alias a normal message's request ID.
	identity := sha256.Sum256([]byte(sid + "\x00" + claim.Message.Input.RequestID))
	claim.ExecutionRequestID = fmt.Sprintf("queue_%x", identity)
	claim.TurnID = fmt.Sprintf("turn_queue_%x", identity)
	claim.AfterRequestID = latestRequest
	var collision int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM turn_inputs WHERE session_id=? AND (request_id=? OR turn_id=?)`, sid, claim.ExecutionRequestID, claim.TurnID).Scan(&collision); err != nil {
		return out, err
	}
	if collision > 0 {
		return out, session.ErrQueuedMessageConflict
	}
	now := time.Now().UTC()
	payload, err := json.Marshal(struct{ Kind, SessionID, TurnID, RequestID string }{"user_queue_claimed", sid, claim.TurnID, claim.Message.Input.RequestID})
	if err != nil {
		return out, err
	}
	event, err := tx.ExecContext(ctx, `INSERT INTO session_events(session_id,turn_id,kind,at,payload) VALUES(?,?,?,?,?)`, sid, claim.TurnID, "user_queue_claimed", formatTime(now), string(payload))
	if err != nil {
		return out, err
	}
	claim.EventSequence, err = event.LastInsertId()
	if err != nil {
		return out, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO user_queue_claims(session_id,request_id,execution_request_id,turn_id,after_request_id,event_seq) VALUES(?,?,?,?,?,?)`, sid, claim.Message.Input.RequestID, claim.ExecutionRequestID, claim.TurnID, latestRequest, claim.EventSequence); err != nil {
		return out, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE user_queue_messages SET state='claimed' WHERE session_id=? AND request_id=?`, sid, claim.Message.Input.RequestID); err != nil {
		return out, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE user_queue_control SET revision=revision+1 WHERE session_id=?`, sid); err != nil {
		return out, err
	}
	claim.Message.State = "claimed"
	out.Claim = &claim
	out.Revision++
	return out, nil
}

func (q *UserQueue) unfinishedQueueInput(ctx context.Context, tx *sql.Tx, sid string) (string, error) {
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM runtime_continuations WHERE session_id=? AND checkpoint!='' AND state NOT IN ('finished','stopped','superseded')`, sid).Scan(&count); err != nil {
		return "", err
	}
	if count > 0 {
		return "background_wait", nil
	}
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM runtime_inputs WHERE session_id=? AND state='pending' AND kind!=''`, sid).Scan(&count); err != nil {
		return "", err
	}
	if count > 0 {
		return "runtime_input_pending", nil
	}
	// Supplements are optional and may never have been installed in this store.
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='user_supplement_boundaries'`).Scan(&count); err != nil {
		return "", err
	}
	if count > 0 {
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM user_supplement_boundaries WHERE session_id=? AND sealed=0`, sid).Scan(&count); err != nil {
			return "", err
		}
		if count > 0 {
			return "supplement_boundary_open", nil
		}
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM user_supplements WHERE session_id=? AND state='pending_apply'`, sid).Scan(&count); err != nil {
			return "", err
		}
		if count > 0 {
			return "supplement_pending", nil
		}
	}
	return "", nil
}

var _ session.UserQueueDeliveryStore = (*UserQueue)(nil)
