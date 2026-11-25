-- Prep evidence (WP-1150, WP-ADR157): beside a task prep's pointers, the
-- recorded data that bears on the task and the claims in its text worth
-- testing. One row per task, replaced when the prep is made again; a row with
-- nothing in it says the task needed none.
CREATE TABLE task_prep_evidence (
    issue_id   INTEGER PRIMARY KEY REFERENCES issues(id) ON DELETE CASCADE,
    text_hash  TEXT    NOT NULL,             -- of the task text that was read
    state      TEXT    NOT NULL,             -- pending (in flight, or lost), ready, failed, refused
    notes      TEXT    NOT NULL DEFAULT '',  -- JSON {evidence, claims}
    model      TEXT    NOT NULL DEFAULT '',
    request_id TEXT    NOT NULL DEFAULT '',
    candidates INTEGER NOT NULL DEFAULT 0,
    offered    TEXT    NOT NULL DEFAULT '[]',
    dropped    INTEGER NOT NULL DEFAULT 0,   -- items the model named that didn't check out
    error      TEXT    NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL              -- of this state
);
-- For the collector's health check (WP-999): the preps of late, and the
-- newest evidence made.
CREATE INDEX task_prep_evidence_state ON task_prep_evidence(state, created_at);
CREATE INDEX task_preps_updated ON task_preps(updated_at);

-- A unit of the prep_evidence experiment as it was at first exposure, for
-- both arms, so later edits and re-preps don't change what is compared.
CREATE TABLE prep_evidence_units (
    run_id     INTEGER NOT NULL REFERENCES experiment_runs(id) ON DELETE CASCADE,
    issue_id   INTEGER NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
    issue_type TEXT    NOT NULL,
    priority   INTEGER NOT NULL,
    text_len   INTEGER NOT NULL,
    text_hash  TEXT    NOT NULL,
    candidates INTEGER NOT NULL,
    evidence_n INTEGER NOT NULL,
    claims_n   INTEGER NOT NULL,
    dropped    INTEGER NOT NULL,
    risk       TEXT    NOT NULL,             -- the task text's risk level
    evidence   TEXT    NOT NULL,             -- JSON {evidence, claims} as first exposed
    model      TEXT    NOT NULL,
    at         INTEGER NOT NULL,
    PRIMARY KEY (run_id, issue_id)
);
CREATE INDEX prep_evidence_units_issue ON prep_evidence_units(issue_id);

-- Ends at 100 finished tasks in each arm.
INSERT INTO experiment_runs(experiment, weights, target, note, started_at)
VALUES ('prep_evidence', '{"none":1,"shown":1}', 100, 'started by WP-1150', unixepoch() * 1000);
