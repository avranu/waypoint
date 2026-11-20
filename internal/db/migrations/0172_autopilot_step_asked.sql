-- A step a person asked for in one of their own sessions (Start next task,
-- WP-1319). Autopilot otherwise moves on only sessions it started.
ALTER TABLE autopilot_steps ADD COLUMN asked INTEGER NOT NULL DEFAULT 0;
