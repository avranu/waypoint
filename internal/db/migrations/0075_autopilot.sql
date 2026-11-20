-- Autopilot (WP-194, WP-ADR9): a person turns it on per project, and when an
-- agent session of theirs there has finished its task, Waypoint clears the
-- session and starts the next task in it, with that task's wp next answer as
-- the prompt. Limits (last hour's spend, the week's and the five hours'
-- quota) pause it; a person's own prompt keeps the session theirs.

-- A person's settings: one row per project they set, plus one with no
-- project for their defaults. limits is JSON; a field left out inherits
-- (project row, then the defaults row, then Waypoint's defaults).
CREATE TABLE autopilot_settings (
    id         INTEGER PRIMARY KEY,
    user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    project_id INTEGER REFERENCES projects(id) ON DELETE CASCADE,
    enabled    INTEGER NOT NULL DEFAULT 0,
    limits     TEXT    NOT NULL DEFAULT '{}',
    updated_at INTEGER NOT NULL
);
CREATE UNIQUE INDEX autopilot_settings_key ON autopilot_settings(user_id, coalesce(project_id, 0));

-- What autopilot is doing about one finished session: due (it starts the
-- next task at due_at), waiting (reason says on what; looked at again at
-- due_at), starting (runner job job_id clears it and pastes the prompt), and
-- the ends: started (next_session took over), stopped (the person took the
-- session back, or turned autopilot off), failed.
CREATE TABLE autopilot_steps (
    id           INTEGER PRIMARY KEY,
    user_id      INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    project_id   INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    session_id   INTEGER NOT NULL REFERENCES agent_sessions(id) ON DELETE CASCADE,
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
    updated_at   INTEGER NOT NULL
);
CREATE UNIQUE INDEX autopilot_steps_session ON autopilot_steps(session_id);
CREATE INDEX autopilot_steps_due ON autopilot_steps(state, due_at);
CREATE INDEX autopilot_steps_user ON autopilot_steps(user_id, updated_at);

-- The agent's last message after it finished a task (every session, not
-- only autopilot's), for the person to read later. asks: "waiting on you"
-- lines in it that no request of the session covered; request_id: the
-- request filed for them.
CREATE TABLE task_summaries (
    id          INTEGER PRIMARY KEY,
    user_id     INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    project_id  INTEGER REFERENCES projects(id) ON DELETE SET NULL,
    session_id  INTEGER NOT NULL REFERENCES agent_sessions(id) ON DELETE CASCADE,
    run_id      INTEGER NOT NULL UNIQUE REFERENCES agent_runs(id) ON DELETE CASCADE,
    issue_id    INTEGER REFERENCES issues(id) ON DELETE SET NULL,
    agent       TEXT    NOT NULL DEFAULT '',
    body        TEXT    NOT NULL,
    asks        TEXT    NOT NULL DEFAULT '[]',
    request_id  INTEGER REFERENCES requests(id) ON DELETE SET NULL,
    autopilot   INTEGER NOT NULL DEFAULT 0, -- autopilot moved the session on after it
    created_at  INTEGER NOT NULL,
    updated_at  INTEGER NOT NULL,
    reviewed_at INTEGER,
    reviewed_by INTEGER REFERENCES users(id) ON DELETE SET NULL
);
CREATE INDEX task_summaries_user ON task_summaries(user_id, reviewed_at, updated_at);
CREATE INDEX task_summaries_session ON task_summaries(session_id);

-- Background tasks a session's Stops reported, by signature, with when each
-- was first seen: one started before the task finished, or running for
-- hours, is stale and doesn't hold autopilot.
ALTER TABLE agent_sessions ADD COLUMN bg_seen TEXT NOT NULL DEFAULT '{}';
