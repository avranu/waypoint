-- A pause that also winds down running sessions (WP-146): besides taking
-- no new work, the person's sessions start no agents, finish the step
-- they're on, write a hand-off and stop. The quota alert's Pause sets it.
ALTER TABLE agent_pauses ADD COLUMN sessions INTEGER NOT NULL DEFAULT 0;
