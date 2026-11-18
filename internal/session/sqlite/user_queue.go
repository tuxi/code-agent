package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"code-agent/internal/session"
)

// UserQueue is explicitly constructed; production admission and consumption
// remain disabled until their shared finish/claim boundary is implemented.
type UserQueue struct{ store *Store }

func NewUserQueue(store *Store) (*UserQueue, error) {
	_, err := store.db.Exec(`
 CREATE TABLE IF NOT EXISTS user_queue_control (
 session_id TEXT PRIMARY KEY, revision INTEGER NOT NULL DEFAULT 0,
 next_sequence INTEGER NOT NULL DEFAULT 1, state TEXT NOT NULL DEFAULT 'enabled',
 pause_reason TEXT NOT NULL DEFAULT '');
 CREATE TABLE IF NOT EXISTS user_queue_messages (
 session_id TEXT NOT NULL, request_id TEXT NOT NULL, sequence INTEGER NOT NULL,
 accepted_revision INTEGER NOT NULL, payload_hash TEXT NOT NULL, payload TEXT NOT NULL,
 state TEXT NOT NULL DEFAULT 'queued', created_at TEXT NOT NULL,
 PRIMARY KEY(session_id,request_id), UNIQUE(session_id,sequence));
 CREATE TRIGGER IF NOT EXISTS user_queue_session_delete AFTER DELETE ON sessions BEGIN
 DELETE FROM user_queue_messages WHERE session_id=OLD.id;
 DELETE FROM user_queue_control WHERE session_id=OLD.id;
 END;`)
	if err != nil {
		return nil, err
	}
	if err = ensureUserQueueDeliverySchema(store.db); err != nil {
		return nil, err
	}
	return &UserQueue{store: store}, nil
}

func queuedMessagePayload(in session.QueuedMessage) ([]byte, string, error) {
	c := in.Context
	for _, field := range []string{in.SelectionEffort, in.SessionID, in.RequestID, c.WireModel, c.ResolvedModel, c.ReasoningEffort, c.WorkspaceRoot, c.WorkspaceRel, c.WorkspaceExtID, c.WorkspacePath, c.AuthorizationScope, c.ApprovalMode} {
		if !utf8.ValidString(field) || len(field) > 4096 || strings.ContainsRune(field, 0) {
			return nil, "", session.ErrInvalidQueuedMessage
		}
	}
	if strings.TrimSpace(in.SessionID) == "" || len(in.SessionID) > 256 || strings.TrimSpace(in.RequestID) == "" || len(in.RequestID) > 256 ||
		!utf8.ValidString(in.Text) || strings.TrimSpace(in.Text) == "" || utf8.RuneCountInString(in.Text) > 100000 ||
		strings.TrimSpace(c.WireModel) == "" || strings.TrimSpace(c.ResolvedModel) == "" || strings.TrimSpace(c.AuthorizationScope) == "" || strings.TrimSpace(c.ApprovalMode) == "" {
		return nil, "", session.ErrInvalidQueuedMessage
	}
	switch c.WorkspaceRoot {
	case "":
		if c.WorkspaceRel != "" || c.WorkspaceExtID != "" {
			return nil, "", session.ErrInvalidQueuedMessage
		}
	case session.RootWorkspace:
		if c.WorkspaceRel == "" || c.WorkspaceExtID != "" {
			return nil, "", session.ErrInvalidQueuedMessage
		}
	case session.RootExternal:
		if c.WorkspaceExtID == "" || c.WorkspaceRel != "" {
			return nil, "", session.ErrInvalidQueuedMessage
		}
	default:
		return nil, "", session.ErrInvalidQueuedMessage
	}
	body, err := json.Marshal(in)
	if err != nil {
		return nil, "", err
	}
	hash := sha256.Sum256(body)
	return body, hex.EncodeToString(hash[:]), nil
}

func (q *UserQueue) EnqueueUserMessage(ctx context.Context, in session.QueuedMessage) (session.QueuedMessageReceipt, bool, error) {
	body, hash, err := queuedMessagePayload(in)
	if err != nil {
		return session.QueuedMessageReceipt{}, false, err
	}
	tx, err := q.store.db.BeginTx(ctx, nil)
	if err != nil {
		return session.QueuedMessageReceipt{}, false, err
	}
	defer tx.Rollback()
	// Serialize enqueue with archive/delete before inspecting session identity.
	result, err := tx.ExecContext(ctx, `UPDATE sessions SET updated_at=updated_at WHERE id=?`, in.SessionID)
	if err != nil {
		return session.QueuedMessageReceipt{}, false, err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return session.QueuedMessageReceipt{}, false, err
	}
	if n == 0 {
		return session.QueuedMessageReceipt{}, false, session.ErrUserQueueUnavailable
	}
	var storedHash string
	receipt, err := scanQueuedMessage(tx.QueryRowContext(ctx, `SELECT payload,sequence,accepted_revision,state,created_at,payload_hash FROM user_queue_messages WHERE session_id=? AND request_id=?`, in.SessionID, in.RequestID), &storedHash)
	if err == nil {
		if hash != storedHash {
			return session.QueuedMessageReceipt{}, false, session.ErrQueuedMessageConflict
		}
		return receipt, false, tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return session.QueuedMessageReceipt{}, false, err
	}
	var path, root, rel, ext, archived string
	err = tx.QueryRowContext(ctx, `SELECT COALESCE(workspace_path,''),COALESCE(workspace_root,''),COALESCE(workspace_rel,''),COALESCE(workspace_ext_id,''),COALESCE(archived_at,'') FROM sessions WHERE id=?`, in.SessionID).Scan(&path, &root, &rel, &ext, &archived)
	if err != nil {
		return session.QueuedMessageReceipt{}, false, err
	}
	c := in.Context
	if archived != "" || c.WorkspaceRoot != root || c.WorkspaceRel != rel || c.WorkspaceExtID != ext || c.WorkspacePath != path {
		return session.QueuedMessageReceipt{}, false, session.ErrUserQueueUnavailable
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO user_queue_control(session_id) VALUES(?) ON CONFLICT(session_id) DO NOTHING`, in.SessionID); err != nil {
		return session.QueuedMessageReceipt{}, false, err
	}
	var seq, revision int64
	if err = tx.QueryRowContext(ctx, `SELECT next_sequence,revision FROM user_queue_control WHERE session_id=?`, in.SessionID).Scan(&seq, &revision); err != nil {
		return session.QueuedMessageReceipt{}, false, err
	}
	now := time.Now().UTC()
	revision++
	if _, err = tx.ExecContext(ctx, `INSERT INTO user_queue_messages(session_id,request_id,sequence,accepted_revision,payload_hash,payload,created_at) VALUES(?,?,?,?,?,?,?)`, in.SessionID, in.RequestID, seq, revision, hash, string(body), formatTime(now)); err != nil {
		return session.QueuedMessageReceipt{}, false, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE user_queue_control SET next_sequence=?,revision=? WHERE session_id=?`, seq+1, revision, in.SessionID); err != nil {
		return session.QueuedMessageReceipt{}, false, err
	}
	if err = tx.Commit(); err != nil {
		return session.QueuedMessageReceipt{}, false, err
	}
	// Decode our frozen payload rather than returning any caller-owned references.
	receipt = session.QueuedMessageReceipt{Sequence: seq, Revision: revision, State: "queued", CreatedAt: now}
	err = json.Unmarshal(body, &receipt.Input)
	return receipt, true, err
}

type queueScanner interface{ Scan(...any) error }

func scanQueuedMessage(row queueScanner, hash *string) (session.QueuedMessageReceipt, error) {
	var r session.QueuedMessageReceipt
	var body, at string
	if err := row.Scan(&body, &r.Sequence, &r.Revision, &r.State, &at, hash); err != nil {
		return r, err
	}
	if err := json.Unmarshal([]byte(body), &r.Input); err != nil {
		return r, err
	}
	var err error
	r.CreatedAt, err = time.Parse(time.RFC3339Nano, at)
	return r, err
}

func (q *UserQueue) QueuedUserMessage(ctx context.Context, sid, rid string) (session.QueuedMessageReceipt, error) {
	var hash string
	return scanQueuedMessage(q.store.db.QueryRowContext(ctx, `SELECT q.payload,q.sequence,q.accepted_revision,q.state,q.created_at,q.payload_hash FROM user_queue_messages q JOIN sessions s ON s.id=q.session_id WHERE q.session_id=? AND q.request_id=?`, sid, rid), &hash)
}

func (q *UserQueue) ListUserQueue(ctx context.Context, sid string, cursor *session.UserQueueCursor, limit int) (session.UserQueuePage, error) {
	page := session.UserQueuePage{Items: []session.QueuedMessageReceipt{}, State: "enabled"}
	if sid == "" || limit < 0 || limit > 100 || (cursor != nil && (cursor.SessionID != sid || cursor.Revision < 0 || cursor.AfterSequence < 0)) {
		return page, session.ErrInvalidQueuedMessage
	}
	if limit == 0 {
		limit = 50
	}
	tx, err := q.store.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return page, err
	}
	defer tx.Rollback()
	var exists int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM sessions WHERE id=?`, sid).Scan(&exists); err != nil {
		return page, err
	}
	if exists == 0 {
		return page, session.ErrUserQueueUnavailable
	}
	err = tx.QueryRowContext(ctx, `SELECT revision,state,pause_reason FROM user_queue_control WHERE session_id=?`, sid).Scan(&page.Revision, &page.State, &page.PauseReason)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return page, err
	}
	after := int64(0)
	if cursor != nil {
		if cursor.Revision != page.Revision {
			return page, session.ErrUserQueueRevision
		}
		after = cursor.AfterSequence
	}
	rows, err := tx.QueryContext(ctx, `SELECT payload,sequence,accepted_revision,state,created_at,payload_hash FROM user_queue_messages WHERE session_id=? AND sequence>? AND state IN ('queued','claimed') ORDER BY sequence LIMIT ?`, sid, after, limit+1)
	if err != nil {
		return page, err
	}
	for rows.Next() {
		var hash string
		item, e := scanQueuedMessage(rows, &hash)
		if e != nil {
			rows.Close()
			return page, e
		}
		page.Items = append(page.Items, item)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return page, err
	}
	if len(page.Items) > limit {
		page.Items = page.Items[:limit]
		page.Next = &session.UserQueueCursor{SessionID: sid, Revision: page.Revision, AfterSequence: page.Items[len(page.Items)-1].Sequence}
	}
	return page, tx.Commit()
}

var _ session.UserQueueStore = (*UserQueue)(nil)
