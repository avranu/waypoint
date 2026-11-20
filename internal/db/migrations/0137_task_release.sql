-- What a hold waits for besides a date, as JSON (WP-776, WP-ADR75).
ALTER TABLE issues ADD COLUMN hold_when TEXT NOT NULL DEFAULT '';
CREATE INDEX issues_hold_when ON issues(id) WHERE hold_when != '';

-- A task given back on purpose: by wp release, or by a hold that ended the
-- agent's claim. agent_runs.outcome = 'released' is also what the claim
-- sweeps write, so it can't tell these apart. until_at is when a release
-- with no condition lets the task be handed out again; NULL when a hold
-- decides that instead.
CREATE TABLE task_releases (
  id INTEGER PRIMARY KEY,
  issue_id INTEGER NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
  agent TEXT NOT NULL DEFAULT '',
  session_id INTEGER REFERENCES agent_sessions(id) ON DELETE SET NULL,
  run_id INTEGER REFERENCES agent_runs(id) ON DELETE SET NULL,
  released_at INTEGER NOT NULL,
  until_at INTEGER
);
CREATE INDEX task_releases_issue ON task_releases(issue_id, released_at);
CREATE INDEX task_releases_run ON task_releases(run_id) WHERE run_id IS NOT NULL;
CREATE INDEX task_releases_until ON task_releases(until_at) WHERE until_at IS NOT NULL;
