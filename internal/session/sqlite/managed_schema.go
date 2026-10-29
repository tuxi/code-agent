package sqlite

import "database/sql"

func ensureManagedTaskSchema(tx *sql.Tx) error {
	_, err := tx.Exec(`
CREATE TABLE IF NOT EXISTS coding_task_runs (id TEXT PRIMARY KEY, task_id TEXT NOT NULL, active INTEGER NOT NULL DEFAULT 1, document TEXT NOT NULL);
CREATE UNIQUE INDEX IF NOT EXISTS idx_coding_task_active_run ON coding_task_runs(task_id) WHERE active=1;
CREATE TABLE IF NOT EXISTS coding_task_commands (id TEXT PRIMARY KEY, task_id TEXT NOT NULL, run_id TEXT NOT NULL, document TEXT NOT NULL, receipt TEXT);
CREATE TABLE IF NOT EXISTS coding_task_dispatches (id TEXT PRIMARY KEY, task_id TEXT NOT NULL, run_id TEXT NOT NULL, session_id TEXT NOT NULL, request_id TEXT NOT NULL, iteration INTEGER NOT NULL, turn_id TEXT, document TEXT NOT NULL, UNIQUE(session_id,request_id), UNIQUE(run_id,iteration));
CREATE TABLE IF NOT EXISTS coding_task_workspace_claims (root TEXT PRIMARY KEY, task_id TEXT NOT NULL, run_id TEXT NOT NULL UNIQUE);
CREATE TABLE IF NOT EXISTS coding_task_execution_leases (id TEXT PRIMARY KEY, root TEXT NOT NULL, session_id TEXT NOT NULL, request_id TEXT NOT NULL, run_id TEXT NOT NULL);
CREATE UNIQUE INDEX IF NOT EXISTS idx_coding_task_managed_writer ON coding_task_execution_leases(run_id) WHERE run_id != '';
`)
	return err
}
