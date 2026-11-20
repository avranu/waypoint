-- Autopilot's quota limits, turned off for a while by the person: until this
-- time (ms; 0 = on) the week's and five-hour limits hold nothing back. Only
-- the defaults row (project_id NULL) carries it. See core/autopilot.go.
ALTER TABLE autopilot_settings ADD COLUMN quota_off_until INTEGER NOT NULL DEFAULT 0;
