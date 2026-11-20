-- Deploy lag alerts (WP-199): an environment that still runs an older commit
-- than its branch's head this many minutes after the branch moved tells the
-- project's owners (0: never). Production and staging start at 45 minutes.
-- lag_alerted_at is the last alert, so one stall is told once a day, not
-- every minute.
ALTER TABLE environments ADD COLUMN lag_alert_m INTEGER NOT NULL DEFAULT 0;
ALTER TABLE environments ADD COLUMN lag_alerted_at INTEGER;
UPDATE environments SET lag_alert_m = 45 WHERE kind IN ('production', 'staging') AND ephemeral = 0;
