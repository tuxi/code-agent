-- Unused B2-a only. Stop every Runtime, back up both databases, and run inside
-- a maintenance connection. On ANY error issue ROLLBACK and keep the new binary.
-- This refuses to discard any Run, command, reservation, claim or live lease.
BEGIN IMMEDIATE;
CREATE TEMP TABLE b2a_downgrade_guard (empty INTEGER CHECK(empty=1));
INSERT INTO b2a_downgrade_guard SELECT CASE WHEN
    (SELECT value FROM coding_task_meta WHERE key='schema_version') != '2'
 OR EXISTS(SELECT 1 FROM coding_task_runs)
 OR EXISTS(SELECT 1 FROM coding_task_commands)
 OR EXISTS(SELECT 1 FROM coding_task_dispatches)
 OR EXISTS(SELECT 1 FROM coding_task_workspace_claims)
 OR EXISTS(SELECT 1 FROM coding_task_execution_leases)
 THEN 0 ELSE 1 END;
DROP TABLE coding_task_execution_leases;
DROP TABLE coding_task_workspace_claims;
DROP TABLE coding_task_dispatches;
DROP TABLE coding_task_commands;
DROP TABLE coding_task_runs;
UPDATE coding_task_meta SET value='1' WHERE key='schema_version';
DROP TABLE b2a_downgrade_guard;
COMMIT;
