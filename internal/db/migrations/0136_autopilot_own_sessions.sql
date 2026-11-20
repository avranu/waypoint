-- waypoint:no-foreign-keys
-- Autopilot starts sessions of its own (WP-799, WP-ADR72): such a start has
-- no session behind it, so a step's session_id may now be NULL. The unique
-- index still gives a session one step and allows any number of NULLs.
CREATE TABLE autopilot_steps_new (
    id           INTEGER PRIMARY KEY,
    user_id      INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    project_id   INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    session_id   INTEGER REFERENCES agent_sessions(id) ON DELETE CASCADE, -- NULL: autopilot's own start
    run_id       INTEGER REFERENCES agent_runs(id) ON DELETE SET NULL, -- the finished task's run
    stop_at      INTEGER NOT NULL, -- the Stop that found it finished
    state        TEXT    NOT NULL,
    reason       TEXT    NOT NULL DEFAULT '',
    detail       TEXT    NOT NULL DEFAULT '',
    due_at       INTEGER NOT NULL,
    next_issue   INTEGER REFERENCES issues(id) ON DELETE SET NULL,
    job_id       INTEGER, -- the runner job (not a foreign key: old jobs are pruned)
    next_session INTEGER REFERENCES agent_sessions(id) ON DELETE SET NULL,
    attempts     INTEGER NOT NULL DEFAULT 0,
    notified     TEXT    NOT NULL DEFAULT '', -- the reason last told to the person
    created_at   INTEGER NOT NULL,
    updated_at   INTEGER NOT NULL,
    review_id    INTEGER REFERENCES work_log(id) ON DELETE SET NULL,
    agent        TEXT    NOT NULL DEFAULT '' -- autopilot's own start: who its task is reserved as
);
INSERT INTO autopilot_steps_new (id, user_id, project_id, session_id, run_id, stop_at, state, reason, detail,
    due_at, next_issue, job_id, next_session, attempts, notified, created_at, updated_at, review_id)
SELECT id, user_id, project_id, session_id, run_id, stop_at, state, reason, detail,
    due_at, next_issue, job_id, next_session, attempts, notified, created_at, updated_at, review_id
FROM autopilot_steps;
DROP TABLE autopilot_steps;
ALTER TABLE autopilot_steps_new RENAME TO autopilot_steps;
CREATE UNIQUE INDEX autopilot_steps_session ON autopilot_steps(session_id);
CREATE INDEX autopilot_steps_due ON autopilot_steps(state, due_at);
CREATE INDEX autopilot_steps_user ON autopilot_steps(user_id, updated_at);
CREATE INDEX autopilot_steps_next_session ON autopilot_steps(next_session);
