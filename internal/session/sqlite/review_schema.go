package sqlite

import "database/sql"

func ensureReviewSchema(tx *sql.Tx) error {
	_, err := tx.Exec(`
CREATE TABLE IF NOT EXISTS coding_task_artifact_blobs (id TEXT PRIMARY KEY, task_id TEXT NOT NULL, run_id TEXT NOT NULL, kind TEXT NOT NULL, version TEXT NOT NULL, hash TEXT NOT NULL, document BLOB NOT NULL);
CREATE TABLE IF NOT EXISTS coding_task_reviews (id TEXT PRIMARY KEY, task_id TEXT NOT NULL, run_id TEXT NOT NULL, iteration INTEGER NOT NULL, producer TEXT NOT NULL, document TEXT NOT NULL, UNIQUE(run_id,iteration), UNIQUE(run_id,producer));
CREATE TABLE IF NOT EXISTS coding_task_gate_decisions (id TEXT PRIMARY KEY, run_id TEXT NOT NULL, document TEXT NOT NULL);
CREATE TRIGGER IF NOT EXISTS coding_blob_no_update BEFORE UPDATE ON coding_task_artifact_blobs BEGIN SELECT RAISE(ABORT,'immutable review blob'); END;
CREATE TRIGGER IF NOT EXISTS coding_blob_no_delete BEFORE DELETE ON coding_task_artifact_blobs BEGIN SELECT RAISE(ABORT,'immutable review blob'); END;
CREATE TRIGGER IF NOT EXISTS coding_review_no_update BEFORE UPDATE ON coding_task_reviews BEGIN SELECT RAISE(ABORT,'immutable review submission'); END;
CREATE TRIGGER IF NOT EXISTS coding_review_no_delete BEFORE DELETE ON coding_task_reviews BEGIN SELECT RAISE(ABORT,'immutable review submission'); END;
CREATE TRIGGER IF NOT EXISTS coding_gate_no_update BEFORE UPDATE ON coding_task_gate_decisions BEGIN SELECT RAISE(ABORT,'immutable gate decision'); END;
CREATE TRIGGER IF NOT EXISTS coding_gate_no_delete BEFORE DELETE ON coding_task_gate_decisions BEGIN SELECT RAISE(ABORT,'immutable gate decision'); END;
`)
	return err
}
