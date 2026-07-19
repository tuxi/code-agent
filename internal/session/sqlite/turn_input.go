package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"code-agent/internal/model"
	"code-agent/internal/session"
)

var _ session.TurnInputStore = (*Store)(nil)

func (s *Store) ReserveTurnInput(ctx context.Context, input session.TurnInput, accepted session.EventRecord) (session.TurnInput, bool, int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return session.TurnInput{}, false, 0, err
	}
	defer tx.Rollback()
	stored, err := scanTurnInput(tx.QueryRowContext(ctx, `
		SELECT session_id, request_id, turn_id, payload_hash, text, wire_model, resolved_model, assets, state, created_at, updated_at
		FROM turn_inputs WHERE session_id=? AND request_id=?`, input.SessionID, input.RequestID))
	if err == nil {
		return stored, false, 0, tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return session.TurnInput{}, false, 0, err
	}
	assets, err := json.Marshal(input.Assets)
	if err != nil {
		return session.TurnInput{}, false, 0, fmt.Errorf("marshal turn input assets: %w", err)
	}
	now := input.CreatedAt.UTC()
	if now.IsZero() {
		now = time.Now().UTC()
	}
	input.CreatedAt, input.UpdatedAt = now, now
	input.State = session.TurnInputAccepted
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO turn_inputs(session_id, request_id, turn_id, payload_hash, text, wire_model, resolved_model, assets, state, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		input.SessionID, input.RequestID, input.TurnID, input.PayloadHash, input.Text, input.WireModel,
		input.ResolvedModel, string(assets), string(input.State), formatTime(now), formatTime(now)); err != nil {
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
	return scanTurnInput(s.db.QueryRowContext(ctx, `
		SELECT session_id, request_id, turn_id, payload_hash, text, wire_model, resolved_model, assets, state, created_at, updated_at
		FROM turn_inputs WHERE session_id=? AND request_id=?`, sessionID, requestID))
}

func (s *Store) RecoverableTurnInputs(ctx context.Context) ([]session.TurnInput, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT session_id, request_id, turn_id, payload_hash, text, wire_model, resolved_model, assets, state, created_at, updated_at
		FROM turn_inputs WHERE state IN ('accepted','queued','running') ORDER BY created_at, session_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []session.TurnInput
	for rows.Next() {
		input, err := scanTurnInput(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, input)
	}
	return out, rows.Err()
}

type rowScanner interface{ Scan(...any) error }

func scanTurnInput(row rowScanner) (session.TurnInput, error) {
	var input session.TurnInput
	var assets, state, createdAt, updatedAt string
	err := row.Scan(&input.SessionID, &input.RequestID, &input.TurnID, &input.PayloadHash, &input.Text,
		&input.WireModel, &input.ResolvedModel, &assets, &state, &createdAt, &updatedAt)
	if err != nil {
		return session.TurnInput{}, err
	}
	input.State = session.TurnInputState(state)
	input.CreatedAt, input.UpdatedAt = parseTime(createdAt), parseTime(updatedAt)
	if assets != "" {
		if err := json.Unmarshal([]byte(assets), &input.Assets); err != nil {
			return session.TurnInput{}, fmt.Errorf("unmarshal turn input assets: %w", err)
		}
	}
	if input.Assets == nil {
		input.Assets = []model.GatewayAssetRef{}
	}
	return input, nil
}
