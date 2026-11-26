-- WP-1914: a babysit round whose job ended before its agent started (the
-- checkout failed, the runner was lost) isn't counted and goes again after a
-- wait. fault_tries counts such rounds in a row, reset when a round runs;
-- fault_text is what the last one said, for the babysit's wait text.
ALTER TABLE babysits ADD COLUMN fault_tries INTEGER NOT NULL DEFAULT 0;
ALTER TABLE babysits ADD COLUMN fault_text TEXT NOT NULL DEFAULT '';
