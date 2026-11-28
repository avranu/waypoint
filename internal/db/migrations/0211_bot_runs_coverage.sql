-- Record review diff coverage on bot_runs so incomplete reviews cannot turn
-- the review gate green (WP-1434, WP-2021).
ALTER TABLE bot_runs ADD COLUMN dropped_files INTEGER NOT NULL DEFAULT 0;
ALTER TABLE bot_runs ADD COLUMN diff_cut INTEGER NOT NULL DEFAULT 0;
