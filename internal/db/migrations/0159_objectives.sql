-- Objectives (WP-927, WP-ADR111): a task worked in rounds until a check says
-- it is met ("until this runs in under a second"), by an amount every round
-- ("twice as fast", which is never met for good), or as a loop with no
-- check at all. A round ends where the task would close (wp done, or the
-- merge of its pull request); the objective decides whether it does.
CREATE TABLE objectives (
    issue_id         INTEGER PRIMARY KEY REFERENCES issues(id) ON DELETE CASCADE,
    check_cmd        TEXT    NOT NULL DEFAULT '',      -- shell, run in the repository; '': a loop nothing measures
    dir              TEXT    NOT NULL DEFAULT '',      -- the folder within the repository it runs in
    setup            TEXT    NOT NULL DEFAULT '',      -- shell run once in each tree before a timed check
    measure          TEXT    NOT NULL DEFAULT '',      -- time, value, pass; '': none
    until            TEXT    NOT NULL DEFAULT '',      -- the target as text: "< 1s", "2x", "-10%", "pass"
    op               TEXT    NOT NULL DEFAULT '',      -- <, <=, >, >= of a fixed target
    target           REAL,                             -- the fixed target (seconds for time)
    factor           REAL,                             -- a relative target: this many times better than the round's base
    better           TEXT    NOT NULL DEFAULT '',      -- lower or higher
    unit             TEXT    NOT NULL DEFAULT '',      -- a value's unit, as written
    pace             TEXT    NOT NULL DEFAULT 'queue', -- queue: handed out like any task; spare: only on quota that would go unused
    every_ms         INTEGER NOT NULL DEFAULT 0,       -- the least time from one round's end to the next's start
    max_rounds       INTEGER NOT NULL DEFAULT 0,       -- 0: no limit
    round_no         INTEGER NOT NULL DEFAULT 1,       -- the round under way, from 1
    round_started_at INTEGER NOT NULL,
    stalled          INTEGER NOT NULL DEFAULT 0,       -- rounds in a row that moved nothing
    next_at          INTEGER,                          -- not handed out before
    state            TEXT    NOT NULL DEFAULT 'active', -- proposed (an agent's, no person confirmed it), active, met, ended
    set_by           INTEGER REFERENCES users(id) ON DELETE SET NULL,
    set_agent        TEXT    NOT NULL DEFAULT '',
    confirmed_by     INTEGER REFERENCES users(id) ON DELETE SET NULL,
    confirmed_at     INTEGER,
    ended_at         INTEGER,
    created_at       INTEGER NOT NULL,
    updated_at       INTEGER NOT NULL
);
CREATE INDEX objectives_state ON objectives(state);

-- One run of an objective's check, as wp objective check reported it: this
-- tree's result and, when the tree has changed since the base commit, the
-- base's from the same run on the same machine.
CREATE TABLE objective_measurements (
    id         INTEGER PRIMARY KEY,
    issue_id   INTEGER NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
    round_no   INTEGER NOT NULL,
    at         INTEGER NOT NULL,
    ok         INTEGER NOT NULL,            -- the command exited 0
    value      REAL,                        -- NULL: it failed, or printed no number
    base_ok    INTEGER,                     -- NULL: no base was measured
    base_value REAL,
    base_sha   TEXT    NOT NULL DEFAULT '',
    tree       TEXT    NOT NULL DEFAULT '', -- the git tree measured, uncommitted changes included
    commit_sha TEXT    NOT NULL DEFAULT '',
    runs       INTEGER NOT NULL DEFAULT 1,
    low        REAL,                        -- the middle half of a timed check's runs
    high       REAL,
    p          REAL,                        -- how likely a timed change from the base is noise
    moved      INTEGER NOT NULL DEFAULT 0,  -- files changed while it ran
    met        INTEGER NOT NULL DEFAULT 0,
    improved   INTEGER NOT NULL DEFAULT 0,
    agent      TEXT    NOT NULL DEFAULT '',
    user_id    INTEGER REFERENCES users(id) ON DELETE SET NULL,
    session_id INTEGER REFERENCES agent_sessions(id) ON DELETE SET NULL
);
CREATE INDEX objective_measurements_issue ON objective_measurements(issue_id, id);

-- A finished round: what closed it and what its last check read.
CREATE TABLE objective_rounds (
    id             INTEGER PRIMARY KEY,
    issue_id       INTEGER NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
    round_no       INTEGER NOT NULL,
    started_at     INTEGER NOT NULL,
    ended_at       INTEGER NOT NULL,
    outcome        TEXT    NOT NULL,            -- met, progress, none, unmeasured, loop
    measurement_id INTEGER REFERENCES objective_measurements(id) ON DELETE SET NULL,
    agent          TEXT    NOT NULL DEFAULT '',
    pr_url         TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX objective_rounds_issue ON objective_rounds(issue_id, round_no);
