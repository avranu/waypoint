-- Keeping prompt caches warm (WP-192, WP-ADR34). stopped_at: when the
-- session's last turn ended (NULL while a turn runs), the time its prompt
-- cache was last written. cache_ttl: the cache tier its last calls asked for
-- ('1h', '5m'; '' not known). warm_since: the last stop of a real turn, so
-- a run of keep-warm pings is counted from it; warm_pings: pings since then.
-- warm_state: what was decided for the latest stop ('' not yet, 'warm' a
-- ping was sent, 'cool' let it cool, 'compacting', 'compacted', 'failed'),
-- warm_note why, warm_at when. typed_at: the last prompt a person typed into
-- the session (not Waypoint's posts, autopilot's or a ping), prompt_typed
-- whether its latest prompt was one. posted_at: the last time Waypoint
-- posted to its inbox.
ALTER TABLE agent_sessions ADD COLUMN stopped_at INTEGER;
ALTER TABLE agent_sessions ADD COLUMN cache_ttl TEXT NOT NULL DEFAULT '';
ALTER TABLE agent_sessions ADD COLUMN warm_since INTEGER;
ALTER TABLE agent_sessions ADD COLUMN warm_pings INTEGER NOT NULL DEFAULT 0;
ALTER TABLE agent_sessions ADD COLUMN warm_state TEXT NOT NULL DEFAULT '';
ALTER TABLE agent_sessions ADD COLUMN warm_note TEXT NOT NULL DEFAULT '';
ALTER TABLE agent_sessions ADD COLUMN warm_at INTEGER;
ALTER TABLE agent_sessions ADD COLUMN typed_at INTEGER;
ALTER TABLE agent_sessions ADD COLUMN prompt_typed INTEGER NOT NULL DEFAULT 0;
ALTER TABLE agent_sessions ADD COLUMN posted_at INTEGER;
CREATE INDEX agent_sessions_stopped ON agent_sessions(stopped_at) WHERE stopped_at IS NOT NULL AND ended_at IS NULL;
CREATE INDEX agent_sessions_typed ON agent_sessions(user_id, typed_at) WHERE typed_at IS NOT NULL;
