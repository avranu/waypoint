-- Benchmark evidence (WP-935): the wp bench results kept on the exact tree a
-- commit shipped, sent by wp ship beside its test runs. JSON list of
-- {command, runs, wall, cpu, base_ref, base_wall, wall_p, ...}; a "faster"
-- in the ship's summary is compared with it.
ALTER TABLE test_evidence ADD COLUMN bench TEXT NOT NULL DEFAULT '[]';
