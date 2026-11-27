-- When a round last stopped on a usage limit: a quota reset after it ends the wait.
ALTER TABLE babysits ADD COLUMN limited_at INTEGER NOT NULL DEFAULT 0;
