-- Task sessions (WP-34): who started a session decides what Waypoint does.
-- kind: 'task' when a launcher (wp start, a runner) set WP_TASK in its
-- environment, 'yours' for a session a person started; '' for sessions
-- recorded before this. task_keys: the WP_TASK binding (KEY[,KEY…]).
-- context_tokens: the context its last model call carried, from the
-- transcript at each Stop. noted_run: the finished run (agent_runs) the
-- hook last showed the one visible "this session is big" note for.
ALTER TABLE agent_sessions ADD COLUMN kind TEXT NOT NULL DEFAULT '';
ALTER TABLE agent_sessions ADD COLUMN task_keys TEXT NOT NULL DEFAULT '';
ALTER TABLE agent_sessions ADD COLUMN context_tokens INTEGER NOT NULL DEFAULT 0;
ALTER TABLE agent_sessions ADD COLUMN noted_run INTEGER;

CREATE INDEX agent_runs_session_done ON agent_runs(session_id, ended_at) WHERE session_id IS NOT NULL AND outcome = 'done';
