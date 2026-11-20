-- Every agent session has a kind (WP-101). Sessions recorded before 0032
-- got '' and nothing ever set it: a session bound by WP_TASK (task_keys) or
-- started by a runner job is a task session, the rest are the person's own.
-- New sessions always get a kind from the server; the trigger keeps any
-- other insert from leaving it empty.
UPDATE agent_sessions SET kind = 'task' WHERE kind = '' AND (task_keys != '' OR spawned_by_job IS NOT NULL);
UPDATE agent_sessions SET kind = 'yours' WHERE kind = '';

CREATE TRIGGER agent_sessions_kind AFTER INSERT ON agent_sessions WHEN new.kind = '' BEGIN
    UPDATE agent_sessions SET kind = 'yours' WHERE id = new.id;
END;
