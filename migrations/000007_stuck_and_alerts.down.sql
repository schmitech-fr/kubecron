DROP TABLE IF EXISTS alerts_sent;
ALTER TABLE job_runs DROP COLUMN pod_waiting_since;
ALTER TABLE job_runs DROP COLUMN pod_waiting_reason;
