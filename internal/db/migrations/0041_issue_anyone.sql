-- Who can pick a task up (WP-93): issues.agent gains a third state. It was
-- '' (humans only) or 'any' / a spec (delegated to agents); now
--   ''            Anyone: people and agents (the default for new issues)
--   'none'        Humans only
--   'any', spec   Agents (delegated)
-- Every existing '' becomes an explicit 'none', so no old task suddenly
-- lands in an agent's queue.
UPDATE issues SET agent = 'none' WHERE agent = '';

-- The index serves delegated work; humans-only rows aren't part of it.
DROP INDEX IF EXISTS issues_agent;
CREATE INDEX issues_agent ON issues(agent) WHERE agent != '' AND agent != 'none';
