-- Quiet spells of agents' background jobs (WP-1045, WP-ADR118). A runner
-- reports each Claude Code background job that has been quiet (no progress)
-- for a few minutes, again while it stays quiet, and once more when it moves
-- again or ends. Each row is one quiet spell of one job: whether and how its
-- session was told, whether the session read it, and how the spell ended.
-- The decision reads these rows back: how long a kind of job may stay quiet
-- before its session is told, and whether a session whose cache is cold is
-- woken for it, follow from how earlier spells ended.
CREATE TABLE stalled_jobs (
    id          INTEGER PRIMARY KEY,
    user_id     INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    session_id  INTEGER REFERENCES agent_sessions(id) ON DELETE SET NULL,
    project_id  INTEGER REFERENCES projects(id) ON DELETE SET NULL,
    cli_session TEXT    NOT NULL,            -- Claude Code's session id
    task        TEXT    NOT NULL,            -- the background task's id
    class       TEXT    NOT NULL DEFAULT '', -- the kind of command: "git push", "go test", "loop", …
    command     TEXT    NOT NULL DEFAULT '',
    report      TEXT    NOT NULL DEFAULT '', -- the runner's latest report (JSON): the evidence a notice quotes
    started_at  INTEGER NOT NULL,            -- the job's start
    quiet_from  INTEGER NOT NULL,            -- the spell's start: its last progress
    seen_at     INTEGER NOT NULL,            -- the runner's latest report
    told_at     INTEGER,
    told_how    TEXT    NOT NULL DEFAULT '', -- woke, at_stop, passive, keep_warm, cold_wake
    told_quiet  INTEGER NOT NULL DEFAULT 0,  -- minutes quiet when told
    threshold   INTEGER NOT NULL DEFAULT 0,  -- the quiet minutes that applied when told
    warm        INTEGER NOT NULL DEFAULT 0,  -- its cache was warm when told
    skipped     TEXT    NOT NULL DEFAULT '', -- why it couldn't be told when reported: off, timeout (not evidence while untold)
    message_id  INTEGER REFERENCES session_messages(id) ON DELETE SET NULL,
    woke_at     INTEGER,                     -- a passive notice later woke it (cold_wake)
    settled_at  INTEGER,
    settled     TEXT    NOT NULL DEFAULT '', -- progress, ended, lost (no report for a while)
    outcome     TEXT    NOT NULL DEFAULT ''  -- acted, self, withdrawn, unknown
);
CREATE INDEX stalled_jobs_open ON stalled_jobs(user_id, cli_session, task) WHERE settled_at IS NULL;
CREATE INDEX stalled_jobs_user ON stalled_jobs(user_id, settled_at);
CREATE INDEX stalled_jobs_session ON stalled_jobs(session_id) WHERE settled_at IS NULL;
