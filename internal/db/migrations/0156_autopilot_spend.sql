-- A start of autopilot's own made to use what is left of the week's quota
-- (WP-262): over the week's last hours, when the quota would otherwise
-- go unused, the sweep starts sessions past the project's at_once. Marked
-- so the Autopilot tab can list them and the next such start can wait for
-- the last one to show in the quota readings.
ALTER TABLE autopilot_steps ADD COLUMN spend INTEGER NOT NULL DEFAULT 0;
