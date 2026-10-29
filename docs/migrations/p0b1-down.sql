-- OPTIONAL DESTRUCTIVE CLEANUP, not needed for binary rollback.
-- Stop every Runtime using this DB, back up with SQLite backup (including WAL),
-- deploy the previous binary first. This deletes ALL B1 Task data only.
-- Never run against a user's database without explicit confirmation.
BEGIN IMMEDIATE;
DROP TABLE IF EXISTS coding_task_requests;
DROP TABLE IF EXISTS coding_task_events;
DROP TABLE IF EXISTS coding_task_artifacts;
DROP TABLE IF EXISTS coding_task_attempts;
DROP TABLE IF EXISTS coding_task_participations;
DROP TABLE IF EXISTS coding_task_external_sessions;
DROP TABLE IF EXISTS coding_tasks;
DROP TABLE IF EXISTS coding_task_meta;
COMMIT;
