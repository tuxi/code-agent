package sqlite

import (
	"database/sql"
	"fmt"
	"os"
	"time"
)

// Existing indexes are never dropped/rebuilt on open. Expensive first-time
// creation has its own progress event, so the host can identify a long migration.
func ensureSecondaryIndexes(db *sql.DB) error {
	for _, index := range []struct{ name, definition string }{
		{"idx_session_events_session", `CREATE INDEX IF NOT EXISTS idx_session_events_session ON session_events(session_id, at)`},
		{"idx_session_events_session_seq", `CREATE INDEX IF NOT EXISTS idx_session_events_session_seq ON session_events(session_id, id)`},
		{"idx_session_events_session_kind_seq", `CREATE INDEX IF NOT EXISTS idx_session_events_session_kind_seq ON session_events(session_id, kind, id)`},
		{"idx_sessions_updated_id", `CREATE INDEX IF NOT EXISTS idx_sessions_updated_id ON sessions(COALESCE(updated_at,'') DESC, id DESC)`},
		{"idx_messages_origin_turn", `CREATE UNIQUE INDEX IF NOT EXISTS idx_messages_origin_turn ON messages(session_id, origin_turn_id) WHERE origin_turn_id IS NOT NULL AND origin_turn_id != ''`},
	} {
		var exists bool
		if err := db.QueryRow(`SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='index' AND name=?)`, index.name).Scan(&exists); err != nil {
			return err
		}
		if exists {
			continue
		}
		started := time.Now()
		fmt.Fprintf(os.Stderr, "[startup] phase=sqlite_index index=%s started\n", index.name)
		if _, err := db.Exec(index.definition); err != nil {
			return fmt.Errorf("migrate index %s: %w", index.name, err)
		}
		fmt.Fprintf(os.Stderr, "[startup] phase=sqlite_index index=%s completed elapsed=%s\n", index.name, time.Since(started))
	}
	return nil
}
