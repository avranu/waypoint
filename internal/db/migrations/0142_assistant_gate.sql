-- An assistant's gate beyond its door (WP-459): the header the door reads
-- its key from, and the gate's address as the agent reaches it (where the
-- gate serves its connections as MCP servers). Empty means the site's
-- default (WAYPOINT_ASSISTANT_KEY_HEADER, WAYPOINT_ASSISTANT_AGENT_URL).
ALTER TABLE assistants ADD COLUMN key_header TEXT NOT NULL DEFAULT '';
ALTER TABLE assistants ADD COLUMN agent_url  TEXT NOT NULL DEFAULT '';
