package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"

	"code-agent/internal/collab"
)

func unprovenProcessOwnership(reason string) error {
	return fmt.Errorf("process ownership %s: %w", reason, collab.ErrRecoveryRequired)
}

// JobEventSink persists the same bracket in a child partition and its owning
// Session. A child partition is not a Conversation; absence from sessions alone
// is not orphan evidence. Only an unambiguous, existing, disjoint owner permits
// ignoring that partition. Finish events never prove process quiescence.
func rejectUnownedProcessPartitions(ctx context.Context, tx *sql.Tx, root string) error {
	var missing bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM session_events e LEFT JOIN sessions s ON s.id=e.session_id WHERE s.id IS NULL AND e.kind IN ('job_started','task_started'))`).Scan(&missing); err != nil {
		return err
	}
	if !missing {
		return nil
	}
	type key struct{ kind, child, turn, call string }
	type owner struct {
		session, root string
		reason        string
	}
	owners := map[key]owner{}
	children := []key{}
	// Force the covering kind filter before fetching payloads. A table scan
	// would read unrelated token/output blobs in a large desktop EventStore.
	rows, err := tx.QueryContext(ctx, `SELECT e.session_id,e.turn_id,e.kind,e.payload,s.id,s.workspace_path
		FROM session_events e INDEXED BY idx_session_events_session_kind_seq
		LEFT JOIN sessions s ON s.id=e.session_id WHERE e.kind IN ('job_started','task_started')`)
	if err != nil {
		return err
	}
	defer rows.Close()
	size, count := 0, 0
	for rows.Next() {
		var partition, turn, kind string
		var raw []byte
		var parent, path sql.NullString
		if err = rows.Scan(&partition, &turn, &kind, &raw, &parent, &path); err != nil {
			return err
		}
		size += len(raw)
		count++
		if size > 16<<20 || count > 30000 {
			return unprovenProcessOwnership("scan_limit")
		}
		var e struct{ Kind, SessionID, TurnID, CallID string }
		valid := json.Unmarshal(raw, &e) == nil && e.Kind == kind && e.TurnID == turn && e.SessionID != "" && turn != "" && e.CallID != ""
		if !valid {
			if !parent.Valid {
				return unprovenProcessOwnership("missing_or_invalid_correlation")
			}
			continue
		}
		k := key{kind, e.SessionID, turn, e.CallID}
		if !parent.Valid {
			if partition != e.SessionID {
				return unprovenProcessOwnership("missing_parent_partition")
			}
			children = append(children, k)
			continue
		}
		if partition == e.SessionID {
			continue
		} // a child Session cannot self-attest ownership
		o := owners[k]
		if o.session != "" && o.session != parent.String {
			o.reason = "multiple_parent_sessions"
		}
		o.session = parent.String
		if !path.Valid || !filepath.IsAbs(path.String) {
			o.reason = "unknown_owner_workspace"
		} else {
			o.root = canonicalExecutionPath(path.String)
		}
		owners[k] = o
	}
	if err = rows.Err(); err != nil {
		return err
	}
	for _, k := range children {
		o, ok := owners[k]
		if !ok {
			return unprovenProcessOwnership("missing_parent_bracket")
		}
		if o.reason != "" {
			return unprovenProcessOwnership(o.reason)
		}
		if o.root == "" {
			return unprovenProcessOwnership("unknown_owner_workspace")
		}
		if overlap(root, o.root) {
			return unprovenProcessOwnership("owner_workspace_overlap")
		}
	}
	return nil
}
