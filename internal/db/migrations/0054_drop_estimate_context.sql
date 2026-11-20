-- Repowise is removed (WP-30): nothing gathers or reads estimate context any more.
-- Its runner jobs go too, so a queued one is not handed to a runner for nothing.
DELETE FROM runner_jobs WHERE source = 'estimate_context';
DROP TABLE estimate_context;
