-- Per-job Waypoint tokens (WP-418, WP-ADR28 §7.3): a runner job that runs
-- wp gets a token of its own, minted when a runner claims it and deleted
-- when it ends, instead of the runner's token. runner_job_id marks such a
-- token; it goes with its job.
ALTER TABLE api_tokens ADD COLUMN runner_job_id INTEGER REFERENCES runner_jobs(id) ON DELETE CASCADE;
CREATE INDEX api_tokens_runner_job ON api_tokens(runner_job_id) WHERE runner_job_id IS NOT NULL;
