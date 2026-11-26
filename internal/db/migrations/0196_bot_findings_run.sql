-- A review run's findings are read by run_id on a pull request's page, in
-- the review step and in agent activity.
CREATE INDEX IF NOT EXISTS bot_findings_run ON bot_findings(run_id);
