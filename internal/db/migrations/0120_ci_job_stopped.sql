-- A cancelled CI job's own verdict (WP-513). A workflow that fails fast has
-- the forge mark the job that failed cancelled too; only its log tells it
-- from a job a newer push or a person stopped. Waypoint read that log only
-- for heads whose checks it records, so the CI doctor counted such failures
-- as cancels whenever that read never happened (a main push, a head no
-- longer followed, a log read before it was whole). Each recent
-- cancelled job's log is now read once and its verdict kept here:
-- NULL not read yet, 1 a step failed first, 0 only stopped, -1 unreadable
-- (a later attempt replaced its log, or the forge has none).
ALTER TABLE ci_jobs ADD COLUMN step_failed INTEGER;
CREATE INDEX ci_jobs_cancelled_unread ON ci_jobs(forge_id, repo, ended_at) WHERE status = 'cancelled' AND step_failed IS NULL;
