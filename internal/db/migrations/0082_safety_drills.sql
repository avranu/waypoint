-- The safety drill (WP-278, WP-ADR10): once a week known-bad and plain
-- agent-written task texts go through the screen's models, so a model that
-- drifted, was swapped or was talked out of a flag shows up. One row per
-- drill, one per text it read; verdicts never reach task_safety.
CREATE TABLE safety_drills (
    id          INTEGER PRIMARY KEY,
    started_by  INTEGER REFERENCES users(id) ON DELETE SET NULL, -- NULL: the weekly schedule
    started_at  INTEGER NOT NULL,
    finished_at INTEGER NOT NULL,
    passed      INTEGER NOT NULL,
    failed      INTEGER NOT NULL, -- a bad text cleared or a plain one held
    errors      INTEGER NOT NULL  -- texts no model answered for
);
CREATE INDEX safety_drills_started ON safety_drills(started_at);

CREATE TABLE safety_drill_cases (
    drill_id INTEGER NOT NULL REFERENCES safety_drills(id) ON DELETE CASCADE,
    name     TEXT    NOT NULL,
    want     TEXT    NOT NULL,             -- hold, clear
    verdict  TEXT    NOT NULL DEFAULT '',  -- '' when no model answered
    flags    TEXT    NOT NULL DEFAULT '[]',
    reason   TEXT    NOT NULL DEFAULT '',
    model    TEXT    NOT NULL DEFAULT '',  -- the model that answered
    error    TEXT    NOT NULL DEFAULT '',
    pass     INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (drill_id, name)
);
CREATE INDEX safety_drill_cases_model ON safety_drill_cases(model);
