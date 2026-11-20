-- Requests that close themselves (WP-126). A request can watch deterministic
-- state and be closed, with a note saying why, when it is satisfied: a pull
-- request merging, an issue done, a URL answering, a command succeeding on a
-- poll. Waypoint checks without a model; the last check is kept to show.
CREATE TABLE request_watches (
    id           INTEGER PRIMARY KEY,
    request_id   INTEGER NOT NULL REFERENCES requests(id) ON DELETE CASCADE,
    kind         TEXT    NOT NULL,             -- pr-merged, issue-done, url-ok, cmd-ok
    target       TEXT    NOT NULL,             -- owner/repo#15, WP-12, a URL, a command
    ref_id       INTEGER NOT NULL DEFAULT 0,   -- the issue's id
    every_sec    INTEGER NOT NULL DEFAULT 0,   -- polled kinds: how often
    machine      TEXT    NOT NULL DEFAULT '',  -- cmd-ok: where, and in which directory
    dir          TEXT    NOT NULL DEFAULT '',
    run_by       INTEGER REFERENCES users(id) ON DELETE SET NULL, -- cmd-ok: whose runner
    job_id       INTEGER NOT NULL DEFAULT 0,   -- cmd-ok: the poll in flight
    next_at      INTEGER NOT NULL DEFAULT 0,
    checked_at   INTEGER,
    note         TEXT    NOT NULL DEFAULT '',  -- what the last check found
    satisfied_at INTEGER,
    created_at   INTEGER NOT NULL
);
CREATE INDEX request_watches_request ON request_watches(request_id);
CREATE INDEX request_watches_open ON request_watches(kind, next_at) WHERE satisfied_at IS NULL;
