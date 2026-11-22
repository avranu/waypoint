-- The stop rule of review rounds weighs the round and how many serious
-- findings there are (WP-1569). weight: what the round's signals added up
-- to (a medium 1, a high or critical 2); need: the weight another round
-- needed after this one (1 after the first round, then 2, 4, 8 and on).
-- Rows from before had no such rule: 0 for both, and a signal called.
ALTER TABLE review_again_decisions ADD COLUMN need INTEGER NOT NULL DEFAULT 0;
ALTER TABLE review_again_decisions ADD COLUMN weight INTEGER NOT NULL DEFAULT 0;
