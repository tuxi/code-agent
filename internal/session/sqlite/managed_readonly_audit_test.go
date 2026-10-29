package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"testing"
	"time"

	"code-agent/internal/collab"
)

// Opt-in host diagnostic: never migrates, reserves, dispatches or writes an
// existing Store. The pinned Store/Session/root must all match before scanning.
func TestManagedReadOnlyOwnershipAudit(t *testing.T) {
	path := os.Getenv("CODEAGENT_AUDIT_DB")
	if path == "" {
		t.Skip("explicit read-only desktop audit only")
	}
	storeID, sessionID, root := os.Getenv("CODEAGENT_AUDIT_STORE"), os.Getenv("CODEAGENT_AUDIT_SESSION"), os.Getenv("CODEAGENT_AUDIT_ROOT")
	if storeID == "" || sessionID == "" || root == "" {
		t.Fatal("all audit identity fields are required")
	}
	s, err := NewReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	var actualStore, actualRoot string
	if err = tx.QueryRowContext(ctx, `SELECT value FROM coding_task_meta WHERE key='store_id'`).Scan(&actualStore); err != nil {
		t.Fatal(err)
	}
	if err = tx.QueryRowContext(ctx, `SELECT workspace_path FROM sessions WHERE id=?`, sessionID).Scan(&actualRoot); err != nil {
		t.Fatal(err)
	}
	if actualStore != storeID || canonicalExecutionPath(actualRoot) != canonicalExecutionPath(root) {
		t.Fatal("audit identity mismatch")
	}
	started := time.Now()
	err = rejectUnprovenWorkspaceProcesses(ctx, tx, root)
	if err != nil && !errors.Is(err, collab.ErrRecoveryRequired) {
		t.Fatal(err)
	}
	t.Logf("read_only=true ownership_check_passed=%t recovery_required=%t elapsed=%s reason=%v", err == nil, errors.Is(err, collab.ErrRecoveryRequired), time.Since(started), err)
}
