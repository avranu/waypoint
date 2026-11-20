-- A babysit re-runs a head's failed checks once before it hands out a round
-- for them (WP-392): which head, when, and what the failures said, so a
-- re-run that passes files the flaky test with the log of its failure.
ALTER TABLE babysits ADD COLUMN flake_rerun TEXT NOT NULL DEFAULT '';
