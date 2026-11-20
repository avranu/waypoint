-- How long each Go package's tests took in a CI job, and whether go test's
-- -timeout stopped them (WP-922): a package creeping up on its limit, or a
-- job on its timeout-minutes, is found before it fails every pull request
-- (on 2026-11-11 internal/core reached 30m on shard 1 and timed out the
-- front of the merge queue round after round). Read from the job's log with
-- its test evidence; kept as long as the evidence (30 days).
CREATE TABLE ci_job_packages (
    forge_id INTEGER NOT NULL REFERENCES forges(id) ON DELETE CASCADE,
    repo     TEXT    NOT NULL,
    task_id  INTEGER NOT NULL,              -- the job (ci_jobs.task_id)
    package  TEXT    NOT NULL,              -- repo-relative, e.g. internal/core
    outcome  TEXT    NOT NULL,              -- ok, fail, timeout
    ms       INTEGER NOT NULL,              -- go test's own time for it
    limit_ms INTEGER NOT NULL DEFAULT 0,    -- a timeout: the -timeout that fired
    running  TEXT    NOT NULL DEFAULT '',   -- a timeout: the tests it was running, comma-separated
    at       INTEGER NOT NULL,              -- ms, the job's end
    PRIMARY KEY (forge_id, repo, task_id, package)
);
CREATE INDEX ci_job_packages_at ON ci_job_packages(forge_id, repo, at);
