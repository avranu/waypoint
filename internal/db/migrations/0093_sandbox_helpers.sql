-- What each runner machine's sandbox helper last said (WP-418, WP-ADR28,
-- PL7 §5.3): installed or not, the key it pins, its store and its machine
-- users. Waypoint signs job specs only for a runner whose helper is ready.
CREATE TABLE sandbox_helpers (
    runner_id   INTEGER PRIMARY KEY REFERENCES runners(id) ON DELETE CASCADE,
    report      TEXT    NOT NULL,   -- sandbox.Report as JSON
    reported_at INTEGER NOT NULL
);
