package sqlite

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"time"

	"code-agent/internal/session"
)

// ReserveUserQueueClaim bridges into the ordinary durable inbox in the same
// transaction as claim validation. No caller may allocate a replacement turn.
func (q *UserQueue) ReserveUserQueueClaim(ctx context.Context, sid, rid string, current session.QueueExecutionContext) (session.TurnInput, bool, int64, error) {
	empty := session.TurnInput{}
	tx, err := q.deliveryTransaction(ctx, sid)
	if err != nil {
		return empty, false, 0, err
	}
	defer tx.Rollback()
	advanced, err := q.advanceUserQueueTx(ctx, tx, sid)
	if err != nil {
		return empty, false, 0, err
	}
	if advanced.Claim == nil || advanced.Claim.Message.Input.RequestID != rid {
		if advanced.State == "paused" {
			if err = tx.Commit(); err != nil {
				return empty, false, 0, err
			}
		}
		return empty, false, 0, session.ErrUserQueueUnavailable
	}
	claim := advanced.Claim
	frozen := claim.Message.Input.Context
	var path, root, rel, ext string
	if err = tx.QueryRowContext(ctx, `SELECT COALESCE(workspace_path,''),COALESCE(workspace_root,''),COALESCE(workspace_rel,''),COALESCE(workspace_ext_id,'') FROM sessions WHERE id=?`, sid).Scan(&path, &root, &rel, &ext); err != nil {
		return empty, false, 0, err
	}
	if current != frozen || path != frozen.WorkspacePath || root != frozen.WorkspaceRoot || rel != frozen.WorkspaceRel || ext != frozen.WorkspaceExtID {
		if _, err = tx.ExecContext(ctx, `UPDATE user_queue_control SET state='paused',pause_reason='execution_context_changed',revision=revision+1 WHERE session_id=?`, sid); err != nil {
			return empty, false, 0, err
		}
		if err = tx.Commit(); err != nil {
			return empty, false, 0, err
		}
		return empty, false, 0, session.ErrUserQueueContextChanged
	}
	// Use the normal input identity shape. Effort and authorization are retained
	// separately in the accepted event so recovery cannot substitute defaults.
	identity, _ := json.Marshal(struct {
		Text        string `json:"text"`
		Model       string `json:"model"`
		Assets      []any  `json:"assets"`
		LocalAssets []any  `json:"local_assets"`
	}{claim.Message.Input.Text, frozen.WireModel, []any{}, []any{}})
	input := session.TurnInput{SessionID: sid, RequestID: claim.ExecutionRequestID, TurnID: claim.TurnID, Text: claim.Message.Input.Text, WireModel: frozen.WireModel, ResolvedModel: frozen.ResolvedModel, PayloadHash: fmt.Sprintf("%x", sha256.Sum256(identity))}
	now := time.Now().UTC()
	body, err := json.Marshal(struct {
		Kind, SessionID, TurnID, RequestID string
		At                                 time.Time
		QueueContext                       session.QueueExecutionContext `json:"queue_context"`
		QueueRequestID                     string                        `json:"queue_request_id"`
	}{"turn_accepted", sid, input.TurnID, input.RequestID, now, frozen, rid})
	if err != nil {
		return empty, false, 0, err
	}
	stored, created, seq, err := q.store.reserveTurnInputTx(ctx, tx, input, session.EventRecord{SessionID: sid, TurnID: input.TurnID, Kind: "turn_accepted", At: now, Payload: body})
	if err != nil {
		return empty, false, 0, err
	}
	// ReserveTurnInput normally reconciles an existing identity. Here the claim
	// additionally requires exact content, model and provenance compatibility.
	if stored.TurnID != input.TurnID || stored.PayloadHash != input.PayloadHash || stored.Text != input.Text || stored.WireModel != input.WireModel || stored.ResolvedModel != input.ResolvedModel || stored.Provenance != nil || stored.ManagedTask != nil || len(stored.Assets)+len(stored.LocalAssets) > 0 {
		return empty, false, 0, session.ErrQueuedMessageConflict
	}
	if !created {
		var payload string
		if err = tx.QueryRowContext(ctx, `SELECT payload FROM session_events WHERE id=? AND session_id=? AND turn_id=? AND kind='turn_accepted'`, stored.AcceptedSeq, sid, stored.TurnID).Scan(&payload); err != nil {
			return empty, false, 0, err
		}
		var previous struct {
			Context   session.QueueExecutionContext `json:"queue_context"`
			RequestID string                        `json:"queue_request_id"`
		}
		if json.Unmarshal([]byte(payload), &previous) != nil || previous.Context != frozen || previous.RequestID != rid {
			return empty, false, 0, session.ErrQueuedMessageConflict
		}
		seq = stored.AcceptedSeq
	}
	if err = tx.Commit(); err != nil {
		return empty, false, 0, err
	}
	return stored, created, seq, nil
}

var _ session.UserQueueAdmissionStore = (*UserQueue)(nil)

// StartUserQueueClaim is called only after acquiring the scheduler permit and
// re-resolving Current. The prompt, running state and started receipt commit
// together. A running input cannot be started again through this port.
func (q *UserQueue) StartUserQueueClaim(ctx context.Context, input session.TurnInput, sess *session.Session, current session.QueueExecutionContext) (int64, error) {
	tx, err := q.deliveryTransaction(ctx, input.SessionID)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if sess == nil || sess.ID != input.SessionID {
		return 0, session.ErrInvalidQueuedMessage
	}
	var rid, state, body, queueState, path, root, rel, ext string
	err = tx.QueryRowContext(ctx, `SELECT c.request_id,i.state,m.payload,q.state,COALESCE(s.workspace_path,''),COALESCE(s.workspace_root,''),COALESCE(s.workspace_rel,''),COALESCE(s.workspace_ext_id,'') FROM user_queue_claims c JOIN turn_inputs i ON i.session_id=c.session_id AND i.request_id=c.execution_request_id JOIN user_queue_messages m ON m.session_id=c.session_id AND m.request_id=c.request_id JOIN user_queue_control q ON q.session_id=c.session_id JOIN sessions s ON s.id=c.session_id WHERE c.session_id=? AND c.execution_request_id=? AND c.turn_id=? AND c.active=1 AND COALESCE(s.archived_at,'')=''`, input.SessionID, input.RequestID, input.TurnID).Scan(&rid, &state, &body, &queueState, &path, &root, &rel, &ext)
	if err != nil {
		return 0, err
	}
	if queueState != "enabled" || (state != "accepted" && state != "queued") {
		return 0, session.ErrUserQueueUnavailable
	}
	var message session.QueuedMessage
	if err = json.Unmarshal([]byte(body), &message); err != nil {
		return 0, err
	}
	if current != message.Context || path != current.WorkspacePath || root != current.WorkspaceRoot || rel != current.WorkspaceRel || ext != current.WorkspaceExtID {
		if _, err = tx.ExecContext(ctx, `UPDATE user_queue_control SET state='paused',pause_reason='execution_context_changed',revision=revision+1 WHERE session_id=?`, input.SessionID); err != nil {
			return 0, err
		}
		if err = tx.Commit(); err != nil {
			return 0, err
		}
		return 0, session.ErrUserQueueContextChanged
	}
	if input.Text != message.Text || input.WireModel != current.WireModel || input.ResolvedModel != current.ResolvedModel || sess.WorkspacePath != current.WorkspacePath {
		return 0, session.ErrQueuedMessageConflict
	}
	sessCopy := *sess
	sessCopy.ReasoningEffort = current.ReasoningEffort
	sessCopy.SetTurnStatus(session.TurnStatusRunning)
	if err = q.store.startTurnInputTx(ctx, tx, input, &sessCopy); err != nil {
		return 0, err
	}
	now := time.Now().UTC()
	payload, _ := json.Marshal(struct {
		Kind, SessionID, TurnID, RequestID string
		At                                 time.Time
	}{"user_queue_started", input.SessionID, input.TurnID, input.RequestID, now})
	result, err := tx.ExecContext(ctx, `INSERT INTO session_events(session_id,turn_id,kind,at,payload) VALUES(?,?,?,?,?)`, input.SessionID, input.TurnID, "user_queue_started", formatTime(now), string(payload))
	if err != nil {
		return 0, err
	}
	seq, err := result.LastInsertId()
	if err != nil {
		return 0, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE user_queue_messages SET state='started' WHERE session_id=? AND request_id=?`, input.SessionID, rid); err != nil {
		return 0, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE user_queue_control SET revision=revision+1 WHERE session_id=?`, input.SessionID); err != nil {
		return 0, err
	}
	if err = tx.Commit(); err != nil {
		return 0, err
	}
	sess.ReasoningEffort = current.ReasoningEffort
	return seq, nil
}

func (q *UserQueue) PauseUserQueueContext(ctx context.Context, sid string) error {
	tx, err := q.deliveryTransaction(ctx, sid)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `UPDATE user_queue_control SET state='paused',pause_reason='execution_validation_failed',revision=revision+1 WHERE session_id=? AND state!='paused'`, sid); err != nil {
		return err
	}
	return tx.Commit()
}
