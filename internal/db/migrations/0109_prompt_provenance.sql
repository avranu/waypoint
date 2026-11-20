-- Who wrote a prompt Waypoint sends a session (WP-560, WP-ADR41): the
-- agent that wrote it ('' : a person, or Waypoint's own text) and the
-- automation whose step sent it.
ALTER TABLE session_messages ADD COLUMN agent TEXT NOT NULL DEFAULT '';
ALTER TABLE session_messages ADD COLUMN automation_id INTEGER REFERENCES automations(id) ON DELETE SET NULL;

-- The agent that wrote an automation's current rule ('' : a person). A
-- person saving a new rule clears it; turning it on or off doesn't.
ALTER TABLE automations ADD COLUMN rule_agent TEXT NOT NULL DEFAULT '';

-- Until now an agent's save left no saver (WP-D18) and no name: those
-- rules read as an unnamed agent's until a person saves them.
UPDATE automations SET rule_agent = 'agent' WHERE saved_by IS NULL AND created_by IS NOT NULL;
