-- /wp stop (WP-562): when the person stopped Waypoint in a session, so
-- autopilot, keep-warm pings, compaction and posted news leave it alone
-- until /wp continue or /wp next. NULL: Waypoint works with it as usual.
ALTER TABLE agent_sessions ADD COLUMN left_alone_at INTEGER;

-- /wp review (WP-562): how an audit's task goes at the gateway, as its
-- runner last heard, so a person asking whether a long audit still works
-- reads it here instead of Waypoint asking the gateway inside a hook.
ALTER TABLE gateway_tasks ADD COLUMN status        TEXT    NOT NULL DEFAULT ''; -- queued, running, succeeded, … ('': not heard yet)
ALTER TABLE gateway_tasks ADD COLUMN input_tokens  INTEGER NOT NULL DEFAULT 0;
ALTER TABLE gateway_tasks ADD COLUMN output_tokens INTEGER NOT NULL DEFAULT 0;
ALTER TABLE gateway_tasks ADD COLUMN polled_at     INTEGER NOT NULL DEFAULT 0;  -- its runner last asked how it goes (ms)
ALTER TABLE gateway_tasks ADD COLUMN grew_at       INTEGER NOT NULL DEFAULT 0;  -- its token count last grew (ms)
