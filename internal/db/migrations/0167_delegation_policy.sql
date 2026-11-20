-- The delegation policy (WP-1262, WP-ADR129): how an agent that
-- takes a task is told to split its work between models. Drawn once per
-- task, the first time an agent's wp takes it and can print the arm's text,
-- and kept: none (nothing said: today's way), scouts (Haiku workers search,
-- read and review slices; the agent edits), manager (Haiku scouts, and a
-- Sonnet worker implements and fixes; the agent decides, checks claims and
-- reads the diff). Shares come from what earlier tasks' arms cost and how
-- their work held up (delegation_policy.go); nothing here is polled.
--
-- policy: thompson (drawn at the shares, evidence), best (the agent's usage
-- can't be measured: the best arm, no evidence), excluded (the task's risk
-- is high: nothing said, no evidence). propensity: the drawn arm's share.
-- shares: JSON, every candidate's share when drawn. risk: the task's
-- provenance risk level when drawn; text_len: its title and description in
-- characters (covariates, known before the draw).
CREATE TABLE delegation_assignments (
    issue_id    INTEGER PRIMARY KEY REFERENCES issues(id) ON DELETE CASCADE,
    arm         TEXT    NOT NULL,
    policy      TEXT    NOT NULL,
    propensity  REAL    NOT NULL DEFAULT 1,
    shares      TEXT    NOT NULL DEFAULT '{}',
    risk        TEXT    NOT NULL DEFAULT '',
    text_len    INTEGER NOT NULL DEFAULT 0,
    via         TEXT    NOT NULL DEFAULT '', -- next, take
    assigned_at INTEGER NOT NULL
);
CREATE INDEX delegation_assignments_at ON delegation_assignments(assigned_at);
