package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"code-agent/internal/model"
	"code-agent/internal/session"
)

// ApplySupplements checkpoints the actor-owned session at a balanced history
// boundary. The caller may publish returned receipts only after this commits.
// finalize seals only an empty inbox: newly applied input needs another model
// request, so it leaves the target accepting input.
func (s *SupplementInbox) ApplySupplements(ctx context.Context, sess *session.Session, tid string, finalize bool) ([]session.SupplementApplication, error) {
	if sess == nil || sess.ID == "" || tid == "" {
		return nil, session.ErrInvalidSupplement
	}
	pendingTools := map[string]bool{}
	for _, m := range sess.Messages {
		if m.Role == model.RoleAssistant {
			for _, call := range m.ToolCalls {
				pendingTools[call.ID] = true
			}
		}
		if m.Role == model.RoleTool {
			delete(pendingTools, m.ToolCallID)
		}
	}
	if len(pendingTools) > 0 {
		return nil, session.ErrSupplementUnsafeBoundary
	}
	tx, err := s.store.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `UPDATE user_supplement_boundaries SET sealed=sealed WHERE session_id=? AND turn_id=?`, sess.ID, tid); err != nil {
		return nil, err
	}
	candidate := *sess
	applied, err := s.applySupplementsTx(ctx, tx, &candidate, tid, finalize, true)
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	*sess = candidate
	if len(applied) > 0 && s.store.OnSave != nil {
		s.store.OnSave(sess, s.store.path)
	}
	return applied, nil
}

func (s *SupplementInbox) applySupplementsTx(ctx context.Context, tx *sql.Tx, sess *session.Session, tid string, finalize, deliver bool) ([]session.SupplementApplication, error) {
	var err error
	var open int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM user_supplement_boundaries b JOIN sessions s ON s.id=b.session_id
 JOIN turn_inputs i ON i.session_id=b.session_id AND i.turn_id=b.turn_id
 WHERE b.session_id=? AND b.turn_id=? AND b.sealed=0 AND i.state='running' AND COALESCE(s.archived_at,'')=''`, sess.ID, tid).Scan(&open); err != nil {
		return nil, err
	}
	if open == 0 {
		return nil, session.ErrSupplementTargetClosed
	}
	// These runtime-only receipts survive compaction in session metadata. A stale
	// actor snapshot must not overwrite a checkpoint that already applied input.
	key := "user_supplement_receipts:" + tid
	receipts := map[string]int64{}
	if encoded, ok := sess.Metadata[key].(string); ok {
		if err = json.Unmarshal([]byte(encoded), &receipts); err != nil || receipts == nil {
			return nil, session.ErrSupplementStaleContext
		}
	}
	rows, err := tx.QueryContext(ctx, `SELECT sequence,payload,state FROM user_supplements WHERE session_id=? AND turn_id=? ORDER BY sequence`, sess.ID, tid)
	if err != nil {
		return nil, err
	}
	items := []session.SupplementReceipt{}
	for rows.Next() {
		var seq int64
		var payload, state string
		if err = rows.Scan(&seq, &payload, &state); err != nil {
			rows.Close()
			return nil, err
		}
		item, e := decodeSupplement(seq, payload, state)
		if e != nil {
			rows.Close()
			return nil, e
		}
		items = append(items, item)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	for _, item := range items {
		if item.State == "applied" && receipts[item.Input.RequestID] != item.Sequence {
			return nil, session.ErrSupplementStaleContext
		}
	}
	candidate := *sess
	candidate.Messages = append([]model.Message(nil), sess.Messages...)
	candidate.Metadata = make(map[string]any, len(sess.Metadata)+1)
	for k, v := range sess.Metadata {
		candidate.Metadata[k] = v
	}
	applied := []session.SupplementApplication{}
	now := time.Now().UTC()
	for _, item := range items {
		if item.State != "pending_apply" || !deliver {
			continue
		}
		candidate.Messages = append(candidate.Messages, model.Message{Role: model.RoleUser, Content: item.Input.Text, Assets: item.Input.Assets, LocalAssets: item.Input.LocalAssets})
		receipts[item.Input.RequestID] = item.Sequence
		receipt := session.SupplementApplication{AppliedAt: now, RequestID: item.Input.RequestID, TurnID: tid, Sequence: item.Sequence}
		payload, e := json.Marshal(struct {
			Kind        string
			SessionID   string
			TurnID      string
			RequestID   string
			At          time.Time
			Application session.SupplementApplication `json:"supplement_application"`
		}{"user_supplement_applied", sess.ID, tid, receipt.RequestID, now, receipt})
		if e != nil {
			return nil, e
		}
		result, e := tx.ExecContext(ctx, `INSERT INTO session_events(session_id,turn_id,kind,at,payload) VALUES(?,?,'user_supplement_applied',?,?)`, sess.ID, tid, formatTime(now), string(payload))
		if e != nil {
			return nil, e
		}
		receipt.EventSequence, e = result.LastInsertId()
		if e != nil {
			return nil, e
		}
		if _, e = tx.ExecContext(ctx, `UPDATE user_supplements SET state='applied' WHERE sequence=?`, item.Sequence); e != nil {
			return nil, e
		}
		applied = append(applied, receipt)
	}
	if len(applied) > 0 {
		encoded, e := json.Marshal(receipts)
		if e != nil {
			return nil, e
		}
		candidate.Metadata[key] = string(encoded)
		candidate.UpdatedAt = now
		if err = s.store.saveSessionTx(ctx, tx, &candidate); err != nil {
			return nil, err
		}
	} else if finalize {
		if _, err = tx.ExecContext(ctx, `UPDATE user_supplement_boundaries SET sealed=1 WHERE session_id=? AND turn_id=?`, sess.ID, tid); err != nil {
			return nil, err
		}
	}
	if len(applied) > 0 {
		*sess = candidate
	}
	return applied, nil
}
