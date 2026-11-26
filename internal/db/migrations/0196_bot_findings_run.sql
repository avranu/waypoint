-- A review run's findings are read by run_id on a pull request's page, in
-- the review step and in agent activity, and bot_findings had no index on it,
-- so each read walked the table (WP-1944: about 0.4 s per run in production).
CREATE INDEX IF NOT EXISTS bot_findings_run ON bot_findings(run_id);
