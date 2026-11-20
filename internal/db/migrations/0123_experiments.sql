-- Experiments (WP-254, WP-ADR53): A/B tests of how agents work. The
-- experiments themselves (their arms, where they apply, what they measure)
-- are defined in code; these tables are their runs and who got which arm.

-- A run is one experiment switched on, from started_at until stopped_at.
-- Its weights are fixed for the run: changing them is a new run, so every
-- task in a run was randomised the same way.
CREATE TABLE experiment_runs (
    id          INTEGER PRIMARY KEY,
    experiment  TEXT    NOT NULL,
    weights     TEXT    NOT NULL,                -- JSON {arm: weight}
    params      TEXT    NOT NULL DEFAULT '{}',   -- JSON {param: value}
    projects    TEXT    NOT NULL DEFAULT '[]',   -- JSON [project key]; [] = every project
    target      INTEGER NOT NULL DEFAULT 0,      -- finished tasks per arm to stop at; 0 = no target
    note        TEXT    NOT NULL DEFAULT '',
    started_by  INTEGER REFERENCES users(id) ON DELETE SET NULL,
    started_at  INTEGER NOT NULL,
    ends_at     INTEGER,                         -- stops by itself then; NULL = runs until stopped
    stopped_at  INTEGER,
    stop_reason TEXT    NOT NULL DEFAULT ''
);
-- At most one live run per experiment.
CREATE UNIQUE INDEX experiment_runs_live ON experiment_runs(experiment) WHERE stopped_at IS NULL;

-- A unit is a task in a run: its arm, given once and kept, and when the
-- arm first took effect (exposed_at; NULL = assigned but never reached the
-- point the arms differ at, like a prep no agent fetched).
CREATE TABLE experiment_units (
    run_id      INTEGER NOT NULL REFERENCES experiment_runs(id) ON DELETE CASCADE,
    issue_id    INTEGER NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
    arm         TEXT    NOT NULL,
    via         TEXT    NOT NULL DEFAULT '',     -- what assigned it: prep, next, take, autopilot
    assigned_at INTEGER NOT NULL,
    exposed_at  INTEGER,
    PRIMARY KEY (run_id, issue_id)
);
CREATE INDEX experiment_units_issue ON experiment_units(issue_id);

-- The task prep holdout (WP-212: one task in five made but not shown)
-- becomes the first run of the prep experiment, with the same 4:1 split
-- and every prep made so far as its units.
INSERT INTO experiment_runs(experiment, weights, note, started_at)
VALUES ('prep', '{"shown":4,"hidden":1}', 'carried over from the 1-in-5 holdout of WP-212',
        coalesce((SELECT min(created_at) FROM task_preps), unixepoch() * 1000));

INSERT INTO experiment_units(run_id, issue_id, arm, via, assigned_at, exposed_at)
SELECT r.id, p.issue_id, CASE WHEN p.held_out THEN 'hidden' ELSE 'shown' END, 'prep', p.created_at, p.shown_at
FROM task_preps p, experiment_runs r WHERE r.experiment = 'prep';
