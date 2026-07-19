package sqlite

import (
	"context"
	"time"

	"code-agent/internal/session"
)

var _ session.AssetRefReleaseStore = (*Store)(nil)

func (s *Store) EnqueueAssetRefRelease(ctx context.Context, release session.AssetRefRelease) error {
	now := release.CreatedAt.UTC()
	if now.IsZero() {
		now = time.Now().UTC()
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO asset_ref_release_outbox(session_id, credential_scope, attempts, next_attempt_at, created_at, updated_at)
		VALUES (?, ?, 0, ?, ?, ?) ON CONFLICT(session_id) DO NOTHING`,
		release.SessionID, release.CredentialScope, formatTime(now), formatTime(now), formatTime(now))
	return err
}

func (s *Store) DeleteWithAssetRefRelease(ctx context.Context, sessionID string, release session.AssetRefRelease) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := release.CreatedAt.UTC()
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO asset_ref_release_outbox(session_id, credential_scope, attempts, next_attempt_at, created_at, updated_at)
		VALUES (?, ?, 0, ?, ?, ?) ON CONFLICT(session_id) DO NOTHING`, sessionID, release.CredentialScope, formatTime(now), formatTime(now), formatTime(now)); err != nil {
		return err
	}
	for _, query := range []string{
		`DELETE FROM messages WHERE session_id=?`, `DELETE FROM compactions WHERE session_id=?`,
		`DELETE FROM session_events WHERE session_id=?`, `DELETE FROM turn_inputs WHERE session_id=?`, `DELETE FROM sessions WHERE id=?`,
	} {
		if _, err := tx.ExecContext(ctx, query, sessionID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) PendingAssetRefReleases(ctx context.Context, scope string, now time.Time) ([]session.AssetRefRelease, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT session_id, credential_scope, attempts, next_attempt_at, created_at, updated_at
		FROM asset_ref_release_outbox WHERE credential_scope=? AND next_attempt_at<=? ORDER BY created_at`, scope, formatTime(now.UTC()))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []session.AssetRefRelease
	for rows.Next() {
		var release session.AssetRefRelease
		var next, created, updated string
		if err := rows.Scan(&release.SessionID, &release.CredentialScope, &release.Attempts, &next, &created, &updated); err != nil {
			return nil, err
		}
		release.NextAttemptAt, release.CreatedAt, release.UpdatedAt = parseTime(next), parseTime(created), parseTime(updated)
		out = append(out, release)
	}
	return out, rows.Err()
}

func (s *Store) RetryAssetRefRelease(ctx context.Context, sessionID string, attempts int, next time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE asset_ref_release_outbox SET attempts=?, next_attempt_at=?, updated_at=? WHERE session_id=?`,
		attempts, formatTime(next.UTC()), formatTime(time.Now().UTC()), sessionID)
	return err
}

func (s *Store) CompleteAssetRefRelease(ctx context.Context, sessionID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM asset_ref_release_outbox WHERE session_id=?`, sessionID)
	return err
}
