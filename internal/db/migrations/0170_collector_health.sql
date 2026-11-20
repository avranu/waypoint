-- Collector health (WP-999): what runners and the hourly jobs collect (fix
-- traces, proofs, prep scores) can stop without any error. One row per
-- collector, written by the hourly check: what waits, since when, when it
-- last produced anything, and when admins were last told it was stalled.
CREATE TABLE collector_health (
    name           TEXT PRIMARY KEY,
    stalled_since  INTEGER,           -- ms; the first hourly check that saw the stall (not when work began to wait: oldest_wait_at); NULL when it isn't stalled
    alerted_at     INTEGER,           -- ms; the admins were last told then
    alerted_repos  TEXT NOT NULL DEFAULT '', -- the stalled repos (lower-cased, sorted, newline-joined) that telling named; a stalled repo missing from it is told early
    waiting        INTEGER NOT NULL DEFAULT 0,
    oldest_wait_at INTEGER,           -- ms; when the longest-waiting item began to wait
    last_output_at INTEGER,           -- ms; NULL: it has produced nothing
    checked_at     INTEGER NOT NULL
);
