-- The sandbox escape drill (WP-473, PL7 §13): on each machine whose
-- sandbox helper is ready, weekly and after the helper changes, a runner
-- job starts Contained sessions that try to get out, and records what the
-- sandbox refused. The job's row is pruned after 30 days; this one stays as
-- long as its runner does.
CREATE TABLE sandbox_drills (
    id              INTEGER PRIMARY KEY,
    runner_id       INTEGER NOT NULL REFERENCES runners(id) ON DELETE CASCADE,
    job_id          INTEGER NOT NULL DEFAULT 0,
    reason          TEXT    NOT NULL,           -- first, weekly, helper, retry, person
    started_by      INTEGER REFERENCES users(id) ON DELETE SET NULL,
    helper_version  TEXT    NOT NULL DEFAULT '',
    repo_user       TEXT    NOT NULL DEFAULT '', -- the machine user the sessions ran as
    state           TEXT    NOT NULL DEFAULT 'queued', -- queued, running, passed, failed, error
    passed          INTEGER NOT NULL DEFAULT 0,
    escaped         INTEGER NOT NULL DEFAULT 0,
    failed_controls INTEGER NOT NULL DEFAULT 0,
    inconclusive    INTEGER NOT NULL DEFAULT 0,
    cases           TEXT    NOT NULL DEFAULT '[]',
    error           TEXT    NOT NULL DEFAULT '',
    created_at      INTEGER NOT NULL,
    finished_at     INTEGER
);
CREATE INDEX sandbox_drills_runner ON sandbox_drills(runner_id, id);
