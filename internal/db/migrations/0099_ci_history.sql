-- CI history (WP-490). The CI board reads the forge's last day and keeps it
-- in memory only, so nothing said how CI fared last month. Every finished
-- Forgejo Actions run and job (task) of an attached repo is kept here, read
-- as the board reads them and backfilled once per repo from what the forge
-- still lists, for charts of CI over time. Rows are small and kept.
CREATE TABLE ci_runs (
    forge_id   INTEGER NOT NULL REFERENCES forges(id) ON DELETE CASCADE,
    repo       TEXT    NOT NULL,
    number     INTEGER NOT NULL, -- index_in_repo
    workflow   TEXT    NOT NULL DEFAULT '',
    event      TEXT    NOT NULL DEFAULT '',
    ref        TEXT    NOT NULL DEFAULT '',
    sha        TEXT    NOT NULL DEFAULT '',
    title      TEXT    NOT NULL DEFAULT '',
    tier       TEXT    NOT NULL DEFAULT '', -- draft, or '' for a full run (ciTier)
    status     TEXT    NOT NULL,
    created_at INTEGER NOT NULL,
    started_at INTEGER,
    stopped_at INTEGER,
    PRIMARY KEY (forge_id, repo, number)
);
CREATE INDEX ci_runs_created ON ci_runs(created_at);

CREATE TABLE ci_jobs (
    forge_id   INTEGER NOT NULL REFERENCES forges(id) ON DELETE CASCADE,
    repo       TEXT    NOT NULL,
    task_id    INTEGER NOT NULL,
    run_number INTEGER NOT NULL DEFAULT 0,
    workflow   TEXT    NOT NULL DEFAULT '',
    name       TEXT    NOT NULL,
    event      TEXT    NOT NULL DEFAULT '',
    ref        TEXT    NOT NULL DEFAULT '',
    sha        TEXT    NOT NULL DEFAULT '',
    tier       TEXT    NOT NULL DEFAULT '',
    status     TEXT    NOT NULL, -- success, failure, cancelled, skipped
    started_at INTEGER NOT NULL,
    ended_at   INTEGER NOT NULL,
    wait_s     INTEGER, -- ready to started, when its run's creation was known
    PRIMARY KEY (forge_id, repo, task_id)
);
CREATE INDEX ci_jobs_started ON ci_jobs(started_at);
