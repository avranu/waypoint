-- WP-534: the session's last real Stop said it can be cleared (the ship
-- reply's last line), so nothing more is expected of it: its prompt cache is
-- neither kept warm nor compacted.
ALTER TABLE agent_sessions ADD COLUMN said_clearable INTEGER NOT NULL DEFAULT 0;
