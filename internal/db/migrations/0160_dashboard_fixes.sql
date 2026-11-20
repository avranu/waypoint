-- Dashboard fixes (WP-780): the task "Ask an agent" filed for one of a
-- project's dashboard problems, so the next click finds it even after the
-- task or the project is renamed. A row goes with its task.
CREATE TABLE dashboard_fixes (
    project_id INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    signal     TEXT    NOT NULL,  -- the problem: overdue, stuck, blocked, bottleneck, piling, drift
    issue_id   INTEGER NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
    created_at INTEGER NOT NULL,
    PRIMARY KEY (project_id, signal, issue_id)
);
