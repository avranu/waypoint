-- The drift check (WP-559): a model reads agents' pull requests and each
-- week's work against the project's approved goals.

-- What it flagged, for a person to answer. pr_id NULL: from the weekly read.
CREATE TABLE goal_drift_flags (
    id          INTEGER PRIMARY KEY,
    project_id  INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    pr_id       INTEGER REFERENCES pull_requests(id) ON DELETE CASCADE,
    kind        TEXT    NOT NULL, -- contradicts, technology, untraced, run
    goal_id     INTEGER REFERENCES design_notes(id) ON DELETE SET NULL,
    goal_title  TEXT    NOT NULL DEFAULT '', -- as it read when flagged
    reason      TEXT    NOT NULL DEFAULT '',
    model       TEXT    NOT NULL DEFAULT '',
    created_at  INTEGER NOT NULL,
    answer      TEXT    NOT NULL DEFAULT '', -- '', intent, stop
    answered_by INTEGER REFERENCES users(id) ON DELETE SET NULL,
    answered_at INTEGER
);
CREATE INDEX goal_drift_flags_project ON goal_drift_flags(project_id, answer);
CREATE INDEX goal_drift_flags_pr ON goal_drift_flags(pr_id);

-- The tasks a flag is about. A person's "this is fine" makes each one a
-- person's request in the trace to goals.
CREATE TABLE goal_drift_flag_issues (
    flag_id  INTEGER NOT NULL REFERENCES goal_drift_flags(id) ON DELETE CASCADE,
    issue_id INTEGER NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
    PRIMARY KEY (flag_id, issue_id)
);
CREATE INDEX goal_drift_flag_issues_issue ON goal_drift_flag_issues(issue_id);

-- Each agent pull request's check. Kept apart from the flags, and out of
-- every API: a "clear" is a model's guess, and must not read as evidence
-- to the agents working on it.
CREATE TABLE pr_goal_checks (
    pr_id      INTEGER PRIMARY KEY REFERENCES pull_requests(id) ON DELETE CASCADE,
    input_hash TEXT    NOT NULL,
    verdict    TEXT    NOT NULL DEFAULT 'pending', -- pending, clear, flag, error
    flag_id    INTEGER REFERENCES goal_drift_flags(id) ON DELETE SET NULL,
    error      TEXT    NOT NULL DEFAULT '',
    attempts   INTEGER NOT NULL DEFAULT 0,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);
