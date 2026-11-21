-- What each agent session was actually running with, as observed (WP-1468,
-- WP-ADR145 points 3, 4 and 5). Rows over time, never one overwritten row.
-- The state at a moment is the latest snapshot at or before it, plus the
-- session_observed changes (model, effort, permission mode) after that
-- snapshot.

-- One row per SessionStart (any source: startup, resume, clear, compact,
-- fork), PostCompact and ConfigChange: the full state as the hook saw it then.
-- Model, effort and permission-mode changes between snapshots are rows in
-- session_observed, not new snapshots. A column is NULL when that hook's
-- payload did not carry it, and `missing` says why; `sources` says where each
-- value that is there was seen (hook payload, environment, file on disk,
-- process). A session keeps its first snapshot and its newest 99; rows removed
-- between them are counted in hook_drops as server_cap/snapshot, so for a
-- session past the cap the stretch between the first and the oldest kept row
-- has no snapshot.
-- requested_* is what the launch asked for (the claude process's own
-- arguments); model, effort and permission_mode are what the hook was told.
CREATE TABLE session_snapshots (
    id                        INTEGER PRIMARY KEY,
    session_id                INTEGER NOT NULL REFERENCES agent_sessions(id) ON DELETE CASCADE,
    at                        INTEGER NOT NULL,
    trigger_event             TEXT    NOT NULL,
    trigger_source            TEXT,
    harness                   TEXT,
    entrypoint                TEXT,
    harness_version           TEXT,
    wp_version                TEXT,
    model                     TEXT,
    effort                    TEXT,
    permission_mode           TEXT,
    requested_model           TEXT,
    requested_effort          TEXT,
    requested_permission_mode TEXT,
    git_head                  TEXT,
    git_branch                TEXT,
    git_worktree              INTEGER,
    memory_files              INTEGER,
    memory_bytes              INTEGER,
    memory_index_bytes        INTEGER,
    -- settings files, instruction files, env switches, hook events, skills,
    -- MCP servers and plugins by name: JSON, names and sizes only
    inventory                 TEXT    NOT NULL DEFAULT '{}',
    sources                   TEXT    NOT NULL DEFAULT '{}',
    missing                   TEXT    NOT NULL DEFAULT '{}',
    -- what the hook spent building it
    build_ms                  INTEGER
);
CREATE INDEX session_snapshots_session ON session_snapshots(session_id, at, id);

-- A timeline of observed changes: model, effort, permission_mode (a row only
-- when the value differs from the one before it in time; a late event is put
-- in its place, not taken for the newest), config_change and instruction_file
-- (a row each time). source is the hook event that carried it; detail is
-- extra facts of the row (a model switch's from_model and requested_model).
CREATE TABLE session_observed (
    id         INTEGER PRIMARY KEY,
    session_id INTEGER NOT NULL REFERENCES agent_sessions(id) ON DELETE CASCADE,
    at         INTEGER NOT NULL,
    kind       TEXT    NOT NULL,
    value      TEXT    NOT NULL,
    prev       TEXT,
    source     TEXT    NOT NULL,
    detail     TEXT    NOT NULL DEFAULT '{}'
);
CREATE INDEX session_observed_session ON session_observed(session_id, kind, at, id);

-- Hook data the collection dropped, counted (WP-ADR145 point 5): the CLI's
-- offline outbox (overflow, expired, refused, unqueued) and the server's caps.
CREATE TABLE hook_drops (
    user_id  INTEGER NOT NULL,
    machine  TEXT    NOT NULL,
    reason   TEXT    NOT NULL,
    event    TEXT    NOT NULL,
    dropped  INTEGER NOT NULL DEFAULT 0,
    first_at INTEGER NOT NULL,
    last_at  INTEGER NOT NULL,
    PRIMARY KEY (user_id, machine, reason, event)
);

-- Drop reports already counted, by the client's random report id, so a report
-- sent again after a lost reply is not added twice. Rows older than 30 days
-- are removed as reports come in.
CREATE TABLE hook_drop_reports (
    user_id   INTEGER NOT NULL,
    report_id TEXT    NOT NULL,
    at        INTEGER NOT NULL,
    PRIMARY KEY (user_id, report_id)
);
CREATE INDEX hook_drop_reports_at ON hook_drop_reports(at);
