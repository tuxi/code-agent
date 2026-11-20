package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"code-agent/internal/model"
	"code-agent/internal/session"
)

func (s *Store) nameSeqExpr() string {
	if s.legacyPreview {
		return "0"
	}
	return "COALESCE(conversation_name_seq,0)"
}

func (s *Store) savedPreviewExpr() string {
	if s.legacyPreview {
		return "''"
	}
	return "COALESCE(conversation_preview,'')"
}

// Legacy rows can derive a bounded preview from their first persisted input.
func (s *Store) previewColumns() string {
	saved := s.savedPreviewExpr()
	return saved + `,
	COALESCE((SELECT CASE WHEN length(content)>8000 THEN substr(content,1,4000)||char(10)||substr(content,-4000) ELSE content END
		FROM messages WHERE session_id=sessions.id AND role='user' AND runtime_input=0 AND ` + saved + `='' ORDER BY seq LIMIT 1),''),
	COALESCE((SELECT assets FROM messages WHERE session_id=sessions.id AND role='user' AND runtime_input=0 AND ` + saved + `='' ORDER BY seq LIMIT 1),''),
	COALESCE((SELECT local_assets FROM messages WHERE session_id=sessions.id AND role='user' AND runtime_input=0 AND ` + saved + `='' ORDER BY seq LIMIT 1),'')`
}

func (s *Store) metadataColumns() string {
	return `id, COALESCE(model,''), COALESCE(name,''), COALESCE(workspace_path,''),
	COALESCE(workspace_root,''), COALESCE(workspace_rel,''), COALESCE(workspace_ext_id,''),
	COALESCE(updated_at,''), COALESCE(metadata,''), COALESCE(archived_at,''), ` + s.savedPreviewExpr() + ", " + s.nameSeqExpr()
}

func (s *Store) LoadHeader(ctx context.Context, id string) (*session.Session, error) {
	var header session.Session
	var createdAt, updatedAt, archivedAt, metadata string
	err := s.db.QueryRowContext(ctx, `SELECT id, COALESCE(model,''), COALESCE(name,''),
		COALESCE(workspace_path,''), COALESCE(workspace_root,''), COALESCE(workspace_rel,''),
		COALESCE(workspace_ext_id,''), COALESCE(created_at,''), COALESCE(updated_at,''),
		COALESCE(archived_at,''), COALESCE(metadata,''), `+s.savedPreviewExpr()+`, `+s.nameSeqExpr()+` FROM sessions WHERE id=?`, id).Scan(
		&header.ID, &header.Model, &header.Name, &header.WorkspacePath, &header.Workspace.Root,
		&header.Workspace.Rel, &header.Workspace.ExtID, &createdAt, &updatedAt, &archivedAt, &metadata, &header.Preview, &header.NameSeq)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: %q", session.ErrSessionNotFound, id)
	}
	if err != nil {
		return nil, err
	}
	header.CreatedAt, header.UpdatedAt, header.ArchivedAt = parseTime(createdAt), parseTime(updatedAt), parseTime(archivedAt)
	header.Workspace.AbsHint = header.WorkspacePath
	header.Metadata = map[string]any{}
	if metadata != "" {
		if err := json.Unmarshal([]byte(metadata), &header.Metadata); err != nil {
			return nil, fmt.Errorf("unmarshal metadata: %w", err)
		}
	}
	return &header, nil
}

func (s *Store) LoadConversationPreview(ctx context.Context, id string) (string, error) {
	var preview, text, assets, localAssets string
	err := s.db.QueryRowContext(ctx, `SELECT `+s.previewColumns()+` FROM sessions WHERE id=?`, id).Scan(&preview, &text, &assets, &localAssets)
	if err == nil && preview == "" {
		preview = legacyMessagePreview(text, assets, localAssets)
	}
	return preview, err
}

func legacyMessagePreview(text, assets, localAssets string) string {
	var gateway []model.GatewayAssetRef
	var local []model.LocalAssetRef
	_ = json.Unmarshal([]byte(assets), &gateway)
	_ = json.Unmarshal([]byte(localAssets), &local)
	return session.MessagePreview(text, gateway, local)
}

func (s *Store) ListMetadata(ctx context.Context) ([]session.Meta, error) {
	metas, _, err := s.listMetadata(ctx, `SELECT `+s.metadataColumns()+` FROM sessions ORDER BY updated_at DESC, id DESC`)
	return metas, err
}

func (s *Store) ListSessionPage(ctx context.Context, archived bool, before *session.SessionListCursor, limit int) ([]session.Meta, *session.SessionListCursor, error) {
	if limit < 1 || limit > 200 {
		return nil, nil, fmt.Errorf("limit must be between 1 and 200")
	}
	query := `SELECT ` + s.metadataColumns() + ` FROM sessions WHERE (COALESCE(archived_at,'')!='')=?`
	args := []any{archived}
	if before != nil {
		query += ` AND (COALESCE(updated_at,''),id) < (?,?)`
		args = append(args, before.UpdatedAt, before.ID)
	}
	query += ` ORDER BY COALESCE(updated_at,'') DESC, id DESC LIMIT ?`
	args = append(args, limit+1)
	metas, stamps, err := s.listMetadata(ctx, query, args...)
	if err != nil || len(metas) <= limit {
		return metas, nil, err
	}
	next := &session.SessionListCursor{UpdatedAt: stamps[limit-1], ID: metas[limit-1].ID, Archived: archived}
	return metas[:limit], next, nil
}

func (s *Store) listMetadata(ctx context.Context, query string, args ...any) ([]session.Meta, []string, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var metas []session.Meta
	var stamps []string
	for rows.Next() {
		var m session.Meta
		var updatedAt, metadata, archivedAt string
		if err := rows.Scan(&m.ID, &m.Model, &m.Name, &m.WorkspacePath, &m.Workspace.Root,
			&m.Workspace.Rel, &m.Workspace.ExtID, &updatedAt, &metadata, &archivedAt, &m.Preview, &m.NameSeq); err != nil {
			return nil, nil, err
		}
		m.Workspace.AbsHint = m.WorkspacePath
		m.UpdatedAt, m.ArchivedAt = parseTime(updatedAt), parseTime(archivedAt)
		m.TurnStatus, m.PausedAt = turnLifecycleFromMetadata(metadata)
		_ = json.Unmarshal([]byte(metadata), &m.Metadata)
		metas = append(metas, m)
		stamps = append(stamps, updatedAt)
	}
	return metas, stamps, rows.Err()
}

// PauseInterruptedSessions changes only lifecycle metadata, never loading or
// rewriting messages. The UPDATE is conditional, so a completed session stays
// complete and all other metadata survives the recovery.
func (s *Store) PauseInterruptedSessions(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, `UPDATE sessions
		SET metadata=json_set(metadata,'$.turn_status','paused','$.paused_at',?)
		WHERE json_valid(metadata) AND json_extract(metadata,'$.turn_status') IN ('running','resuming')
		RETURNING id`, time.Now().Unix())
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	closeErr := rows.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if s.OnLifecycleUpdate != nil {
		for _, id := range ids {
			s.OnLifecycleUpdate(id, session.TurnStatusPaused)
		}
	}
	return nil
}

// migrateConversationPreviews keeps list and header reads independent of the
// transcript, including on databases created before previews were stored.
func migrateConversationPreviews(db *sql.DB) error {
	var hasPreview bool
	if err := db.QueryRow(`SELECT EXISTS(SELECT 1 FROM pragma_table_info('sessions') WHERE name='conversation_preview')`).Scan(&hasPreview); err != nil || hasPreview {
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`ALTER TABLE sessions ADD COLUMN conversation_preview TEXT`); err != nil {
		if strings.Contains(err.Error(), "duplicate column") {
			return nil
		}
		return err
	}
	rows, err := tx.Query(`SELECT id, ` + (&Store{}).previewColumns() + ` FROM sessions`)
	if err != nil {
		return err
	}
	previews := make(map[string]string)
	for rows.Next() {
		var id, preview, text, assets, localAssets string
		if err = rows.Scan(&id, &preview, &text, &assets, &localAssets); err != nil {
			rows.Close()
			return err
		}
		if preview = legacyMessagePreview(text, assets, localAssets); preview != "" {
			previews[id] = preview
		}
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err = rows.Close(); err != nil {
		return err
	}
	for id, preview := range previews {
		if _, err = tx.Exec(`UPDATE sessions SET conversation_preview=? WHERE id=?`, preview, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}
