package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"code-agent/internal/session"
)

func admitContinuationWork(ctx context.Context, tx *sql.Tx, sid, tid string) error {
	if _, err := tx.ExecContext(ctx, `UPDATE turn_inputs SET state='cancelled' WHERE session_id=? AND turn_id IN
(SELECT turn_id FROM runtime_continuations WHERE session_id=? AND turn_id!=? AND checkpoint!='' AND state IN ('active','waiting','ready'))`, sid, sid, tid); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO runtime_continuations(session_id,turn_id,state) VALUES(?,?,'active')
ON CONFLICT(session_id) DO UPDATE SET turn_id=excluded.turn_id,state='active',revision=0,await_ids='[]',binding='',checkpoint=''
WHERE runtime_continuations.turn_id!=excluded.turn_id`, sid, tid)
	return err
}

func (s *Store) AdmitContinuationWork(ctx context.Context, sid, tid string) error {
	if sid == "" || tid == "" {
		return session.ErrContinuationStale
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = admitContinuationWork(ctx, tx, sid, tid); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) ParkContinuation(ctx context.Context, sess *session.Session, c session.Continuation) (session.Continuation, error) {
	if sess.ID != c.SessionID || c.TurnID == "" || len(c.Await) == 0 || len(c.Await) > 64 || !json.Valid(c.Binding) || !json.Valid(c.Checkpoint) || len(c.Checkpoint) > 4<<20 {
		return c, errors.New("invalid continuation checkpoint")
	}
	for _, id := range c.Await {
		if id == "" || len(id) > 256 {
			return c, errors.New("invalid continuation input identity")
		}
	}
	await, _ := json.Marshal(c.Await)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return c, err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE runtime_continuations SET revision=revision+1,state='waiting',await_ids=?,binding=?,checkpoint=?
WHERE session_id=? AND turn_id=? AND state='active' AND EXISTS(SELECT 1 FROM sessions WHERE id=? AND COALESCE(archived_at,'')='')`, string(await), string(c.Binding), string(c.Checkpoint), c.SessionID, c.TurnID, c.SessionID)
	if err != nil {
		return c, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return c, session.ErrContinuationStale
	}
	if err = readyContinuation(ctx, tx, c.SessionID); err != nil {
		return c, err
	}
	if err = tx.QueryRowContext(ctx, `SELECT revision,state FROM runtime_continuations WHERE session_id=?`, c.SessionID).Scan(&c.Revision, &c.State); err != nil {
		return c, err
	}
	copySession := *sess
	copySession.Metadata = make(map[string]any, len(sess.Metadata)+1)
	for k, v := range sess.Metadata {
		copySession.Metadata[k] = v
	}
	copySession.SetTurnStatus(session.TurnStatusWaitingBackground)
	copySession.UpdatedAt = time.Now().UTC()
	if err = s.saveSessionTx(ctx, tx, &copySession); err != nil {
		return c, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE turn_inputs SET state='waiting_background',updated_at=? WHERE session_id=? AND turn_id=?`, formatTime(copySession.UpdatedAt), c.SessionID, c.TurnID); err != nil {
		return c, err
	}
	if err = tx.Commit(); err != nil {
		return c, err
	}
	sess.Metadata = copySession.Metadata
	sess.UpdatedAt = copySession.UpdatedAt
	return c, nil
}

// The same transaction checks both sides of the finish/register race. There
// is no process-local "check then subscribe" window.
func readyContinuation(ctx context.Context, tx *sql.Tx, sid string) error {
	_, err := tx.ExecContext(ctx, `UPDATE runtime_continuations SET state='ready' WHERE session_id=? AND state='waiting'
AND EXISTS(SELECT 1 FROM runtime_inputs i JOIN json_each(runtime_continuations.await_ids) a ON i.id=a.value WHERE i.session_id=runtime_continuations.session_id AND i.kind!='')`, sid)
	return err
}

func (s *Store) Continuation(ctx context.Context, sid string) (session.Continuation, error) {
	c := session.Continuation{SessionID: sid}
	var await, binding, checkpoint string
	err := s.db.QueryRowContext(ctx, `SELECT turn_id,revision,state,await_ids,binding,checkpoint FROM runtime_continuations WHERE session_id=?`, sid).Scan(&c.TurnID, &c.Revision, &c.State, &await, &binding, &checkpoint)
	if err != nil {
		return c, err
	}
	if err = json.Unmarshal([]byte(await), &c.Await); err != nil {
		return c, err
	}
	c.Binding = json.RawMessage(binding)
	c.Checkpoint = json.RawMessage(checkpoint)
	return c, nil
}

func (s *Store) ClaimContinuation(ctx context.Context, c session.Continuation) (bool, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE runtime_continuations SET state='active' WHERE session_id=? AND turn_id=? AND revision=? AND state='ready'
AND EXISTS(SELECT 1 FROM sessions WHERE id=? AND COALESCE(archived_at,'')='')`, c.SessionID, c.TurnID, c.Revision, c.SessionID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

func (s *Store) EndContinuation(ctx context.Context, sid, tid, state string) (bool, error) {
	if state != "stopped" && state != "finished" && state != "needs_attention" {
		return false, errors.New("invalid continuation disposition")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var previous, checkpoint string
	err = tx.QueryRowContext(ctx, `SELECT state,checkpoint FROM runtime_continuations WHERE session_id=? AND turn_id=?`, sid, tid).Scan(&previous, &checkpoint)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	res, err := tx.ExecContext(ctx, `UPDATE runtime_continuations SET state=? WHERE session_id=? AND turn_id=? AND (state IN ('active','waiting','ready') OR (state='needs_attention' AND ?='stopped'))`, state, sid, tid, state)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	if n == 1 && state == "stopped" {
		if _, err = tx.ExecContext(ctx, `UPDATE turn_inputs SET state='cancelled' WHERE session_id=? AND turn_id=?`, sid, tid); err != nil {
			return false, err
		}
		if previous == "waiting" || previous == "ready" || previous == "needs_attention" {
			now := time.Now().UTC()
			if _, err = tx.ExecContext(ctx, `UPDATE sessions SET metadata=json_set(COALESCE(NULLIF(metadata,''),'{}'),'$.turn_status','done'),updated_at=? WHERE id=?`, formatTime(now), sid); err != nil {
				return false, err
			}
			var saved struct{ Turn map[string]json.RawMessage }
			_ = json.Unmarshal([]byte(checkpoint), &saved)
			event := map[string]any{"Kind": "turn_cancelled", "SessionID": sid, "TurnID": tid, "At": now, "CancelledReason": "user_requested"}
			for _, key := range []string{"BillingUnits", "ModelBillingUnits", "ToolBillingUnits", "ExecutedToolCalls", "SucceededToolCalls", "BillableToolCalls", "DiffStat"} {
				if value := saved.Turn[key]; len(value) > 0 {
					event[key] = value
				}
			}
			payload, encodeErr := json.Marshal(event)
			if encodeErr != nil {
				return false, encodeErr
			}
			if _, err = tx.ExecContext(ctx, `INSERT INTO session_events(session_id,turn_id,kind,at,payload) SELECT ?,?,'turn_cancelled',?,? WHERE NOT EXISTS(SELECT 1 FROM session_events WHERE session_id=? AND turn_id=? AND kind='turn_cancelled')`, sid, tid, formatTime(now), string(payload), sid, tid); err != nil {
				return false, err
			}
		}
	}
	if err = tx.Commit(); err != nil {
		return false, err
	}
	return n == 1, nil
}

var _ session.ContinuationStore = (*Store)(nil)
