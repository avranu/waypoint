-- WP-275: lineage. An agent's issue text records the task its session was
-- working on when it wrote it (origin_issue_id), and how deep that task's
-- own chain of agent-written tasks ran then (origin_depth: 0 = a person
-- wrote or reviewed it), which stands in for the chain once that task is
-- deleted. Text written before this has no origin: depth 1 at most.
ALTER TABLE issues ADD COLUMN origin_issue_id INTEGER REFERENCES issues(id) ON DELETE SET NULL;
ALTER TABLE issues ADD COLUMN origin_depth INTEGER NOT NULL DEFAULT 0;
-- The tasks written while working on one are screened again when it changes.
CREATE INDEX issues_origin ON issues(origin_issue_id) WHERE origin_issue_id IS NOT NULL;
