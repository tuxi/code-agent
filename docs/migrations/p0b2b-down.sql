-- Stop all runtimes; back up BOTH databases. Use a maintenance connection.
-- Empty B2-b only. On ANY error issue ROLLBACK; keep the new binary.
BEGIN IMMEDIATE;
CREATE TEMP TABLE b2b_downgrade_guard (empty INTEGER CHECK(empty=1));
INSERT INTO b2b_downgrade_guard SELECT CASE WHEN
    (SELECT value FROM coding_task_meta WHERE key='schema_version') != '3'
 OR EXISTS(SELECT 1 FROM coding_task_artifact_blobs)
 OR EXISTS(SELECT 1 FROM coding_task_reviews)
 OR EXISTS(SELECT 1 FROM coding_task_gate_decisions)
 OR EXISTS(SELECT 1 FROM coding_task_runs WHERE json_extract(document,'$.review_policy') IS NOT NULL)
 THEN 0 ELSE 1 END;
DROP TABLE coding_task_gate_decisions;
DROP TABLE coding_task_reviews;
DROP TABLE coding_task_artifact_blobs;
UPDATE coding_task_meta SET value='2' WHERE key='schema_version';
DROP TABLE b2b_downgrade_guard;
COMMIT;
