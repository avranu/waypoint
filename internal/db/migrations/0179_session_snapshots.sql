-- What each agent session was actually running with, as observed (WP-1468,
-- WP-ADR145 points 3, 4 and 5). Rows over time, never one overwritten row:
-- the state at any moment is the latest row at or before it.

-- One full inventory per SessionStart (any source: startup, resume, clear,
-- compact, fork), PostCompact and ConfigChange. A column the source did not
-- carry is NULL, and `missing` says why; `sources` says where each value that
-- is there was seen (hook payload, environment, file on disk, process).
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
-- when the value differs from the session's last one), config_change and
-- instruction_file (a row each time). source is the hook event that carried it.
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
