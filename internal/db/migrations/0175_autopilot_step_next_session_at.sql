-- When a step's next_session took over (WP-1319). A session counts as
-- autopilot's until a person types in it after this; updated_at can't say it,
-- as a step is rewritten after it started (a Stop of the old session revives
-- it, a tick or a settings change moves it on).
ALTER TABLE autopilot_steps ADD COLUMN next_session_at INTEGER;
UPDATE autopilot_steps SET next_session_at = updated_at WHERE next_session IS NOT NULL;
