-- Stop the Flux owner and back up this DB. Roll back the Session and Flux DBs
-- together. On ANY error issue ROLLBACK; never delete nonempty receipts.
BEGIN IMMEDIATE;
CREATE TEMP TABLE b2a_flux_downgrade_guard (empty INTEGER CHECK(empty=1));
INSERT INTO b2a_flux_downgrade_guard SELECT CASE WHEN
 EXISTS(SELECT 1 FROM codeagent_submission_receipts) THEN 0 ELSE 1 END;
DROP TABLE codeagent_submission_receipts;
DROP TABLE b2a_flux_downgrade_guard;
COMMIT;
