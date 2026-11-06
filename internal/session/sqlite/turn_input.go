package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"code-agent/internal/collab"
	"code-agent/internal/model"
	"code-agent/internal/session"
)

var _ session.TurnInputStore = (*Store)(nil)

func (s *Store) SupportsValidationContracts() bool { return true }

func (s *Store) ReserveTurnInput(ctx context.Context, input session.TurnInput, accepted session.EventRecord) (session.TurnInput, bool, int64, error) {
	if input.ValidationContract != nil {
		copy, err := input.ValidationContract.CloneValidated()
		if err != nil {
			return session.TurnInput{}, false, 0, err
		}
		persisted, err := session.AcceptedValidation(accepted, input)
		if err != nil || persisted == nil {
			return session.TurnInput{}, false, 0, session.ErrInvalidValidationContract
		}
		want, _ := copy.Hash()
		got, _ := persisted.Hash()
		if got != want {
			return session.TurnInput{}, false, 0, session.ErrInvalidValidationContract
		}
		input.ValidationContract = copy
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return session.TurnInput{}, false, 0, err
	}
	defer tx.Rollback()
	if input.ManagedTask != nil && (accepted.SessionID != input.SessionID || accepted.TurnID != input.TurnID || accepted.Kind != "turn_accepted") {
		return session.TurnInput{}, false, 0, collab.ErrConflict
	}
	if _, err = tx.ExecContext(ctx, `UPDATE coding_task_meta SET value=value WHERE key='store_id'`); err != nil {
		return session.TurnInput{}, false, 0, err
	}
	if err = checkTurnInputAdmission(ctx, tx, input); err != nil {
		return session.TurnInput{}, false, 0, err
	}
	stored, err := scanTurnInput(tx.QueryRowContext(ctx, `
		SELECT session_id, request_id, turn_id, payload_hash, text, wire_model, resolved_model, assets, local_assets,
		       sender_session_id, sender_turn_id, message_id, correlation_id, intent, accepted_seq, state, created_at, updated_at
		FROM turn_inputs WHERE session_id=? AND request_id=?`, input.SessionID, input.RequestID))
	if err == nil {
		stored.ManagedTask, err = managedRefForInput(ctx, tx, stored.SessionID, stored.RequestID)
		if err != nil {
			return session.TurnInput{}, false, 0, err
		}
		stored.ValidationContract, err = loadValidationContract(ctx, tx, stored)
		if err != nil {
			return session.TurnInput{}, false, 0, err
		}
		return stored, false, 0, tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return session.TurnInput{}, false, 0, err
	}
	assets, err := json.Marshal(input.Assets)
	if err != nil {
		return session.TurnInput{}, false, 0, fmt.Errorf("marshal turn input assets: %w", err)
	}
	localAssets, err := json.Marshal(input.LocalAssets)
	if err != nil {
		return session.TurnInput{}, false, 0, fmt.Errorf("marshal turn input local assets: %w", err)
	}
	now := input.CreatedAt.UTC()
	if now.IsZero() {
		now = time.Now().UTC()
	}
	input.CreatedAt, input.UpdatedAt = now, now
	input.State = session.TurnInputAccepted
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO turn_inputs(session_id, request_id, turn_id, payload_hash, text, wire_model, resolved_model, assets, local_assets,
			sender_session_id, sender_turn_id, message_id, correlation_id, intent, accepted_seq, state, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		input.SessionID, input.RequestID, input.TurnID, input.PayloadHash, input.Text, input.WireModel,
		input.ResolvedModel, string(assets), string(localAssets), provenanceField(input, func(p *session.CrossSessionProvenance) string { return p.SenderSessionID }),
		provenanceField(input, func(p *session.CrossSessionProvenance) string { return p.SenderTurnID }),
		provenanceField(input, func(p *session.CrossSessionProvenance) string { return p.MessageID }),
		provenanceField(input, func(p *session.CrossSessionProvenance) string { return p.CorrelationID }),
		provenanceField(input, func(p *session.CrossSessionProvenance) string { return p.Intent }), 0,
		string(input.State), formatTime(now), formatTime(now)); err != nil {
		return session.TurnInput{}, false, 0, err
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO session_events (session_id, turn_id, kind, at, payload) VALUES (?, ?, ?, ?, ?)`,
		accepted.SessionID, accepted.TurnID, accepted.Kind, formatTime(accepted.At), string(accepted.Payload))
	if err != nil {
		return session.TurnInput{}, false, 0, err
	}
	seq, err := result.LastInsertId()
	if err != nil {
		return session.TurnInput{}, false, 0, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE turn_inputs SET accepted_seq=? WHERE session_id=? AND request_id=?`, seq, input.SessionID, input.RequestID); err != nil {
		return session.TurnInput{}, false, 0, err
	}
	input.AcceptedSeq = seq
	if err = admitContinuationWork(ctx, tx, input.SessionID, input.TurnID); err != nil {
		return session.TurnInput{}, false, 0, err
	}
	if err = bindManagedAttempt(ctx, tx, input); err != nil {
		return session.TurnInput{}, false, 0, err
	}
	if err := tx.Commit(); err != nil {
		return session.TurnInput{}, false, 0, err
	}
	return input, true, seq, nil
}

func (s *Store) StartTurnInput(ctx context.Context, input session.TurnInput, sess *session.Session) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `UPDATE coding_task_meta SET value=value WHERE key='store_id'`); err != nil {
		return err
	}
	if err = checkTurnInputAdmission(ctx, tx, input); err != nil {
		return err
	}
	if err := s.saveSessionTx(ctx, tx, sess); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE turn_inputs SET state=?, updated_at=? WHERE session_id=? AND request_id=?`,
		string(session.TurnInputRunning), formatTime(time.Now().UTC()), input.SessionID, input.RequestID)
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n != 1 {
		return fmt.Errorf("turn input %s/%s not found", input.SessionID, input.RequestID)
	}
	return tx.Commit()
}

func (s *Store) SetTurnInputState(ctx context.Context, sessionID, requestID string, state session.TurnInputState) error {
	_, err := s.db.ExecContext(ctx, `UPDATE turn_inputs SET state=?, updated_at=? WHERE session_id=? AND request_id=?`,
		string(state), formatTime(time.Now().UTC()), sessionID, requestID)
	return err
}

func (s *Store) TurnInput(ctx context.Context, sessionID, requestID string) (session.TurnInput, error) {
	input, err := scanTurnInput(s.db.QueryRowContext(ctx, `
		SELECT session_id, request_id, turn_id, payload_hash, text, wire_model, resolved_model, assets, local_assets,
		       sender_session_id, sender_turn_id, message_id, correlation_id, intent, accepted_seq, state, created_at, updated_at
		FROM turn_inputs WHERE session_id=? AND request_id=?`, sessionID, requestID))
	if err == nil {
		input.ManagedTask, err = managedRefForInput(ctx, s.db, sessionID, requestID)
	}
	if err == nil {
		input.ValidationContract, err = loadValidationContract(ctx, s.db, input)
	}
	return input, err
}

func (s *Store) RecoverableTurnInputs(ctx context.Context) ([]session.TurnInput, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT session_id, request_id, turn_id, payload_hash, text, wire_model, resolved_model, assets, local_assets,
		       sender_session_id, sender_turn_id, message_id, correlation_id, intent, accepted_seq, state, created_at, updated_at
		FROM turn_inputs WHERE state IN ('accepted','queued','running') ORDER BY created_at, session_id`)
	if err != nil {
		return nil, err
	}
	var out []session.TurnInput
	for rows.Next() {
		input, err := scanTurnInput(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, input)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	for i := range out {
		out[i].ManagedTask, err = managedRefForInput(ctx, s.db, out[i].SessionID, out[i].RequestID)
		if err != nil {
			return nil, err
		}
		out[i].ValidationContract, err = loadValidationContract(ctx, s.db, out[i])
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

func loadValidationContract(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, input session.TurnInput) (*session.ValidationContract, error) {
	if input.AcceptedSeq == 0 {
		return nil, nil
	}
	var record session.EventRecord
	var payload string
	err := q.QueryRowContext(ctx, `SELECT session_id, turn_id, kind, COALESCE(payload, '') FROM session_events WHERE id=?`, input.AcceptedSeq).Scan(&record.SessionID, &record.TurnID, &record.Kind, &payload)
	if err != nil {
		return nil, err
	}
	if record.SessionID != input.SessionID || record.TurnID != input.TurnID || record.Kind != "turn_accepted" {
		return nil, session.ErrInvalidValidationContract
	}
	record.Payload = json.RawMessage(payload)
	return session.AcceptedValidation(record, input)
}

type rowScanner interface{ Scan(...any) error }

func scanTurnInput(row rowScanner) (session.TurnInput, error) {
	var input session.TurnInput
	var assets, localAssets, senderSessionID, senderTurnID, messageID, correlationID, intent, state, createdAt, updatedAt string
	err := row.Scan(&input.SessionID, &input.RequestID, &input.TurnID, &input.PayloadHash, &input.Text,
		&input.WireModel, &input.ResolvedModel, &assets, &localAssets, &senderSessionID, &senderTurnID,
		&messageID, &correlationID, &intent, &input.AcceptedSeq, &state, &createdAt, &updatedAt)
	if err != nil {
		return session.TurnInput{}, err
	}
	input.State = session.TurnInputState(state)
	input.CreatedAt, input.UpdatedAt = parseTime(createdAt), parseTime(updatedAt)
	if senderSessionID != "" || messageID != "" || correlationID != "" || intent != "" {
		input.Provenance = &session.CrossSessionProvenance{SenderSessionID: senderSessionID, SenderTurnID: senderTurnID, MessageID: messageID, CorrelationID: correlationID, Intent: intent}
	}
	if assets != "" {
		if err := json.Unmarshal([]byte(assets), &input.Assets); err != nil {
			return session.TurnInput{}, fmt.Errorf("unmarshal turn input assets: %w", err)
		}
	}
	if input.Assets == nil {
		input.Assets = []model.GatewayAssetRef{}
	}
	if localAssets != "" {
		if err := json.Unmarshal([]byte(localAssets), &input.LocalAssets); err != nil {
			return session.TurnInput{}, fmt.Errorf("unmarshal turn input local assets: %w", err)
		}
	}
	if input.LocalAssets == nil {
		input.LocalAssets = []model.LocalAssetRef{}
	}
	return input, nil
}

func provenanceField(input session.TurnInput, field func(*session.CrossSessionProvenance) string) string {
	if input.Provenance == nil {
		return ""
	}
	return field(input.Provenance)
}
