-- When a babysit's state or wait last changed (WP-483): wp ci doctor says how
-- long the front of a merge line has waited on one thing. updated_at moves
-- with every sync, so it can't say. NULL on babysits from before: updated_at
-- stands in.
ALTER TABLE babysits ADD COLUMN wait_at INTEGER;
