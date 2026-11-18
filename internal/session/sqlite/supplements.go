package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"unicode/utf8"

	"code-agent/internal/session"
)

// SupplementInbox is constructed explicitly until the executor, query API and
// event replay are wired. Its presence must not advertise steering support.
type SupplementInbox struct{ store *Store }

func NewSupplementInbox(store *Store) (*SupplementInbox, error) {
	_, err := store.db.Exec(`
 CREATE TABLE IF NOT EXISTS user_supplement_boundaries (
 session_id TEXT NOT NULL, turn_id TEXT NOT NULL, sealed INTEGER NOT NULL DEFAULT 0,
 PRIMARY KEY(session_id,turn_id));
 CREATE TABLE IF NOT EXISTS user_supplements (
 sequence INTEGER PRIMARY KEY AUTOINCREMENT, session_id TEXT NOT NULL,
 request_id TEXT NOT NULL, turn_id TEXT NOT NULL, payload_hash TEXT NOT NULL,
 payload TEXT NOT NULL, state TEXT NOT NULL DEFAULT 'pending_apply',
 UNIQUE(session_id,request_id));
 CREATE INDEX IF NOT EXISTS user_supplements_pending ON user_supplements(session_id,turn_id,state,sequence);`)
	if err != nil {
		return nil, err
	}
	return &SupplementInbox{store: store}, nil
}

func (s *SupplementInbox) OpenSupplementBoundary(ctx context.Context, sid, tid string) error {
	if sid == "" || tid == "" {
		return session.ErrInvalidSupplement
	}
	// The INSERT reads the durable turn and archived state in the same statement.
	result, err := s.store.db.ExecContext(ctx, `INSERT INTO user_supplement_boundaries(session_id,turn_id)
 SELECT ?,? WHERE EXISTS(SELECT 1 FROM turn_inputs i JOIN sessions s ON s.id=i.session_id
 WHERE i.session_id=? AND i.turn_id=? AND i.state='running' AND COALESCE(s.archived_at,'')='')
 ON CONFLICT(session_id,turn_id) DO NOTHING`, sid, tid, sid, tid)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		var open int
		err = s.store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM user_supplement_boundaries b
 JOIN turn_inputs i ON i.session_id=b.session_id AND i.turn_id=b.turn_id
 JOIN sessions s ON s.id=b.session_id
 WHERE b.session_id=? AND b.turn_id=? AND b.sealed=0 AND i.state='running' AND COALESCE(s.archived_at,'')=''`, sid, tid).Scan(&open)
		if err != nil {
			return err
		}
		if open == 0 {
			return session.ErrSupplementTargetClosed
		}
	}
	return nil
}

func supplementPayload(in session.Supplement) ([]byte, string, error) {
	if in.SessionID == "" || len(in.SessionID) > 256 || in.RequestID == "" || len(in.RequestID) > 256 ||
		in.ExpectedTurnID == "" || len(in.ExpectedTurnID) > 256 || !utf8.ValidString(in.Text) ||
		utf8.RuneCountInString(in.Text) > 100000 || len(in.Assets)+len(in.LocalAssets) > 16 ||
		(strings.TrimSpace(in.Text) == "" && len(in.Assets)+len(in.LocalAssets) == 0) {
		return nil, "", session.ErrInvalidSupplement
	}
	payload, err := json.Marshal(in)
	if err != nil {
		return nil, "", err
	}
	hash := sha256.Sum256(payload)
	return payload, hex.EncodeToString(hash[:]), nil
}

func (s *SupplementInbox) AcceptSupplement(ctx context.Context, in session.Supplement) (session.SupplementReceipt, bool, error) {
	payload, hash, err := supplementPayload(in)
	if err != nil {
		return session.SupplementReceipt{}, false, err
	}
	tx, err := s.store.db.BeginTx(ctx, nil)
	if err != nil {
		return session.SupplementReceipt{}, false, err
	}
	defer tx.Rollback()
	// Take the SQLite writer lock before reading either identity or boundary.
	if _, err = tx.ExecContext(ctx, `UPDATE user_supplement_boundaries SET sealed=sealed WHERE session_id=? AND turn_id=?`, in.SessionID, in.ExpectedTurnID); err != nil {
		return session.SupplementReceipt{}, false, err
	}
	var storedHash, body, state string
	var seq int64
	err = tx.QueryRowContext(ctx, `SELECT sequence,payload_hash,payload,state FROM user_supplements WHERE session_id=? AND request_id=?`, in.SessionID, in.RequestID).Scan(&seq, &storedHash, &body, &state)
	if err == nil {
		if storedHash != hash {
			return session.SupplementReceipt{}, false, session.ErrSupplementConflict
		}
		receipt, err := decodeSupplement(seq, body, state)
		if err != nil {
			return receipt, false, err
		}
		return receipt, false, tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return session.SupplementReceipt{}, false, err
	}
	var open int
	err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM user_supplement_boundaries b JOIN sessions s ON s.id=b.session_id
 JOIN turn_inputs i ON i.session_id=b.session_id AND i.turn_id=b.turn_id
 WHERE b.session_id=? AND b.turn_id=? AND b.sealed=0 AND i.state='running' AND COALESCE(s.archived_at,'')=''`, in.SessionID, in.ExpectedTurnID).Scan(&open)
	if err != nil {
		return session.SupplementReceipt{}, false, err
	}
	if open == 0 {
		return session.SupplementReceipt{}, false, session.ErrSupplementTargetClosed
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO user_supplements(session_id,request_id,turn_id,payload_hash,payload) VALUES(?,?,?,?,?)`, in.SessionID, in.RequestID, in.ExpectedTurnID, hash, string(payload))
	if err != nil {
		return session.SupplementReceipt{}, false, err
	}
	seq, err = result.LastInsertId()
	if err != nil {
		return session.SupplementReceipt{}, false, err
	}
	receipt, err := decodeSupplement(seq, string(payload), "pending_apply")
	if err != nil {
		return receipt, false, err
	}
	return receipt, true, tx.Commit()
}

func decodeSupplement(seq int64, payload, state string) (session.SupplementReceipt, error) {
	receipt := session.SupplementReceipt{Sequence: seq, State: state}
	err := json.Unmarshal([]byte(payload), &receipt.Input)
	return receipt, err
}

func (s *SupplementInbox) Supplement(ctx context.Context, sid, rid string) (session.SupplementReceipt, error) {
	var seq int64
	var body, state string
	err := s.store.db.QueryRowContext(ctx, `SELECT sequence,payload,state FROM user_supplements WHERE session_id=? AND request_id=?`, sid, rid).Scan(&seq, &body, &state)
	if err != nil {
		return session.SupplementReceipt{}, err
	}
	return decodeSupplement(seq, body, state)
}

func (s *SupplementInbox) PendingSupplements(ctx context.Context, sid, tid string) ([]session.SupplementReceipt, error) {
	rows, err := s.store.db.QueryContext(ctx, `SELECT sequence,payload,state FROM user_supplements WHERE session_id=? AND turn_id=? AND state='pending_apply' ORDER BY sequence`, sid, tid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []session.SupplementReceipt{}
	for rows.Next() {
		var seq int64
		var body, state string
		if err = rows.Scan(&seq, &body, &state); err != nil {
			return nil, err
		}
		r, e := decodeSupplement(seq, body, state)
		if e != nil {
			return nil, e
		}
		items = append(items, r)
	}
	return items, rows.Err()
}

func (s *SupplementInbox) SealSupplementBoundary(ctx context.Context, sid, tid string) error {
	tx, err := s.store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE user_supplement_boundaries SET sealed=1 WHERE session_id=? AND turn_id=?`, sid, tid)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return session.ErrSupplementTargetClosed
	}
	var pending int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM user_supplements WHERE session_id=? AND turn_id=? AND state='pending_apply'`, sid, tid).Scan(&pending); err != nil {
		return err
	}
	if pending > 0 {
		return session.ErrSupplementPending
	}
	return tx.Commit()
}

var _ session.SupplementStore = (*SupplementInbox)(nil)
