package sqlite

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
)

// Additive, one transaction, same database. No changes to Session tables.
func ensureTaskSchema(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.Exec(`
CREATE TABLE IF NOT EXISTS coding_task_meta (key TEXT PRIMARY KEY,value TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS coding_tasks (id TEXT PRIMARY KEY,owner TEXT NOT NULL,workspace_ref TEXT NOT NULL,document TEXT NOT NULL);
CREATE INDEX IF NOT EXISTS idx_coding_tasks_owner ON coding_tasks(owner,workspace_ref,id);
CREATE TABLE IF NOT EXISTS coding_task_participations (id TEXT PRIMARY KEY,task_id TEXT NOT NULL,session_key TEXT NOT NULL,role TEXT NOT NULL,document TEXT NOT NULL,UNIQUE(task_id,session_key,role));
CREATE TABLE IF NOT EXISTS coding_task_attempts (id TEXT PRIMARY KEY,task_id TEXT NOT NULL,participation_id TEXT NOT NULL,session_id TEXT,turn_id TEXT,submission_id TEXT,checkpoint INTEGER NOT NULL DEFAULT 0,document TEXT NOT NULL,UNIQUE(session_id,turn_id),UNIQUE(task_id,participation_id,submission_id));
CREATE INDEX IF NOT EXISTS idx_coding_task_attempts_task ON coding_task_attempts(task_id,id);
CREATE TABLE IF NOT EXISTS coding_task_artifacts (id TEXT PRIMARY KEY,task_id TEXT NOT NULL,attempt_id TEXT NOT NULL,kind TEXT NOT NULL,version INTEGER NOT NULL,document TEXT NOT NULL);
CREATE INDEX IF NOT EXISTS idx_coding_task_artifacts_task ON coding_task_artifacts(task_id,kind,version);
CREATE TABLE IF NOT EXISTS coding_task_events (seq INTEGER PRIMARY KEY AUTOINCREMENT,event_id TEXT NOT NULL UNIQUE,task_id TEXT NOT NULL,document TEXT NOT NULL);
CREATE INDEX IF NOT EXISTS idx_coding_task_events_task ON coding_task_events(task_id,seq);
CREATE TABLE IF NOT EXISTS coding_task_requests (owner TEXT NOT NULL,scope TEXT NOT NULL,request_id TEXT NOT NULL,payload_hash TEXT NOT NULL,receipt TEXT NOT NULL,PRIMARY KEY(owner,scope,request_id));
CREATE TABLE IF NOT EXISTS coding_task_external_sessions (handle TEXT PRIMARY KEY,owner TEXT NOT NULL,document TEXT NOT NULL);
`)
	if err != nil {
		return err
	}
	if _, err = tx.Exec(`INSERT OR IGNORE INTO coding_task_meta(key,value) VALUES('schema_version','1')`); err != nil {
		return err
	}
	var version string
	if err = tx.QueryRow(`SELECT value FROM coding_task_meta WHERE key='schema_version'`).Scan(&version); err != nil {
		return err
	}
	if version != "1" && version != "2" && version != "3" {
		return errors.New("unsupported coding task schema version")
	}
	if err = ensureManagedTaskSchema(tx); err != nil {
		return err
	}
	if err = ensureReviewSchema(tx); err != nil {
		return err
	}
	if _, err = tx.Exec(`UPDATE coding_task_meta SET value='3' WHERE key='schema_version'`); err != nil {
		return err
	}
	var bytes [16]byte
	if _, err = rand.Read(bytes[:]); err != nil {
		return err
	}
	if _, err = tx.Exec(`INSERT OR IGNORE INTO coding_task_meta(key,value) VALUES('store_id',?)`, "cstore_"+hex.EncodeToString(bytes[:])); err != nil {
		return err
	}
	return tx.Commit()
}
