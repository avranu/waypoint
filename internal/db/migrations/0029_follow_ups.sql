-- Follow-ups (WP-D22): steps queued on an agent session that run once, when
-- the agent truly finishes. A follow-up is an automation bound to a session:
-- hidden from project and admin lists, owned by the session's person.
ALTER TABLE automations ADD COLUMN session_id INTEGER REFERENCES agent_sessions(id) ON DELETE CASCADE;
CREATE UNIQUE INDEX automations_session ON automations(session_id) WHERE session_id IS NOT NULL;

-- What the session's last Stop said it still runs (background tasks, crons
-- and wakeups) in words; '?' when its CLI didn't say. finished_at: the last
-- Stop with nothing pending, no open request and nothing delivered; cleared
-- when the agent works again. spawned_by_job: the runner job (start_session)
-- that started it.
ALTER TABLE agent_sessions ADD COLUMN pending TEXT NOT NULL DEFAULT '';
ALTER TABLE agent_sessions ADD COLUMN finished_at INTEGER;
ALTER TABLE agent_sessions ADD COLUMN spawned_by_job INTEGER;

-- Text for an agent's next prompt (tell_agent). Delivered by the Stop hook,
-- by a runner posting to the session's inbox socket, or at the session's next
-- Stop or SessionStart.
CREATE TABLE session_messages (
    id           INTEGER PRIMARY KEY,
    session_id   INTEGER NOT NULL REFERENCES agent_sessions(id) ON DELETE CASCADE,
    body         TEXT    NOT NULL,
    state        TEXT    NOT NULL DEFAULT 'queued', -- queued, sending (a runner has it), delivered, cancelled
    via          TEXT    NOT NULL DEFAULT '',       -- stop, socket, start
    step_key     TEXT    NOT NULL DEFAULT '',       -- run:step#iteration#attempt, so a step sends once
    created_by   INTEGER REFERENCES users(id) ON DELETE SET NULL,
    created_at   INTEGER NOT NULL,
    delivered_at INTEGER
);
CREATE INDEX session_messages_session ON session_messages(session_id, state);
CREATE UNIQUE INDEX session_messages_step ON session_messages(step_key) WHERE step_key != '';

-- A run another run's step started (run_automation), found again by that
-- step's key so a step run twice after a wait starts it once.
ALTER TABLE automation_runs ADD COLUMN parent_step TEXT NOT NULL DEFAULT '';
CREATE INDEX automation_runs_parent_step ON automation_runs(parent_step) WHERE parent_step != '';
