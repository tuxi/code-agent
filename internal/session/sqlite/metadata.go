package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"code-agent/internal/session"
)

const metadataColumns = `id, COALESCE(model,''), COALESCE(name,''), COALESCE(workspace_path,''),
	COALESCE(workspace_root,''), COALESCE(workspace_rel,''), COALESCE(workspace_ext_id,''),
	COALESCE(updated_at,''), COALESCE(metadata,''), COALESCE(archived_at,'')`

func (s *Store) LoadHeader(ctx context.Context, id string) (*session.Session, error) {
	var header session.Session
	var createdAt, updatedAt, archivedAt, metadata string
	err := s.db.QueryRowContext(ctx, `SELECT id, COALESCE(model,''), COALESCE(name,''),
		COALESCE(workspace_path,''), COALESCE(workspace_root,''), COALESCE(workspace_rel,''),
		COALESCE(workspace_ext_id,''), COALESCE(created_at,''), COALESCE(updated_at,''),
		COALESCE(archived_at,''), COALESCE(metadata,'') FROM sessions WHERE id=?`, id).Scan(
		&header.ID, &header.Model, &header.Name, &header.WorkspacePath, &header.Workspace.Root,
		&header.Workspace.Rel, &header.Workspace.ExtID, &createdAt, &updatedAt, &archivedAt, &metadata)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("session %q not found", id)
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

func (s *Store) ListMetadata(ctx context.Context) ([]session.Meta, error) {
	metas, _, err := s.listMetadata(ctx, `SELECT `+metadataColumns+` FROM sessions ORDER BY updated_at DESC, id DESC`)
	return metas, err
}

func (s *Store) ListSessionPage(ctx context.Context, archived bool, before *session.SessionListCursor, limit int) ([]session.Meta, *session.SessionListCursor, error) {
	if limit < 1 || limit > 200 {
		return nil, nil, fmt.Errorf("limit must be between 1 and 200")
	}
	query := `SELECT ` + metadataColumns + ` FROM sessions WHERE (COALESCE(archived_at,'')!='')=?`
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
			&m.Workspace.Rel, &m.Workspace.ExtID, &updatedAt, &metadata, &archivedAt); err != nil {
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
