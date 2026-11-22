-- The stop rule of review rounds weighs the round and how many serious
-- findings there are (WP-1569). A new high or critical signal calls another
-- round at any round index; medium signals are counted against what the
-- round's index needs. serious: the round's high and critical signals;
-- weight: its medium signals; need: the mediums another round needed after
-- this one (1 after the first round, then 2, 4, 8 and on). Rows from before
-- had no such rule: 0 for all three, and any signal called.
ALTER TABLE review_again_decisions ADD COLUMN need INTEGER NOT NULL DEFAULT 0;
ALTER TABLE review_again_decisions ADD COLUMN weight INTEGER NOT NULL DEFAULT 0;
ALTER TABLE review_again_decisions ADD COLUMN serious INTEGER NOT NULL DEFAULT 0;
