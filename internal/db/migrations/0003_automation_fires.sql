-- Time-based automations (due soon, stale, schedules) fire once per issue
-- and period; this remembers which have.

CREATE TABLE automation_fires (
    automation_id INTEGER NOT NULL REFERENCES automations(id) ON DELETE CASCADE,
    issue_id      INTEGER NOT NULL DEFAULT 0, -- 0: a rule without an issue
    period        TEXT    NOT NULL,
    fired_at      INTEGER NOT NULL,
    PRIMARY KEY (automation_id, issue_id, period)
);
