-- WP-683: a babysit round its agent couldn't run because its owner's usage
-- limit was reached isn't counted; the next round waits until the limit
-- lifts (ms, 0 for none). limited_tries counts such rounds in a row whose
-- reset nothing told, which end up counted (babysit_limit.go);
-- prev_streaks are the streaks before the last round, which a dropped
-- round restores (NULL before this migration).
ALTER TABLE babysits ADD COLUMN limited_until INTEGER NOT NULL DEFAULT 0;
ALTER TABLE babysits ADD COLUMN limited_tries INTEGER NOT NULL DEFAULT 0;
ALTER TABLE babysits ADD COLUMN prev_streaks TEXT;
