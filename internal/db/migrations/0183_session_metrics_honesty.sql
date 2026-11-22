-- Session metrics: stop storing guesses as measurements (WP-1470, WP-ADR145
-- point 4).
--
-- end_inferred: 1 when agent_runs.ended_at is a time the server worked out
-- (the sweep closed a run it never saw end: its lease, or 24 hours after it
-- started), not one it saw. ended_at stays as the bound that "is it still
-- open" queries use; end_inferred says it is not a measurement. Earlier rows
-- closed by that sweep carry outcome expired or timeout, which is how they
-- are told apart here.
ALTER TABLE agent_runs ADD COLUMN end_inferred INTEGER NOT NULL DEFAULT 0;
UPDATE agent_runs SET end_inferred = 1 WHERE outcome IN ('expired', 'timeout') AND ended_at IS NOT NULL;
