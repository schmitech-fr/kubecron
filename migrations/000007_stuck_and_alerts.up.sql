-- Stuck-run detection: the reason a run's pod is blocked (CrashLoopBackOff,
-- ImagePullBackOff, Unschedulable, …) and since when. Written by the pod
-- watcher on change only, cleared when the pod stops waiting. `since` is what
-- tells a pod that has been crash-looping for an hour from one that is
-- restarting once.
ALTER TABLE job_runs ADD COLUMN pod_waiting_reason TEXT;
ALTER TABLE job_runs ADD COLUMN pod_waiting_since DATETIME;

-- Alerts already delivered, one row per (kind, subject). Kept in the database
-- rather than in memory so a restart neither re-sends every recent failure nor
-- forgets a missed run it already reported. Purged with run history.
CREATE TABLE alerts_sent (
    kind    TEXT     NOT NULL,
    subject TEXT     NOT NULL,
    sent_at DATETIME NOT NULL,
    PRIMARY KEY (kind, subject)
);
