-- WP-1320, after 0174 was merged: what the review loop's record needs to
-- read its leads and its decisions back.
--
-- audit_helper_runs.state may now also be partial (some of the check of
-- findings' checks failed) or stopped (its job was cancelled under it).
-- leads: the panel check's leads the reviewer was handed; cut: the ones it
-- wasn't, each with why ([{…,"why"}]: no title or problem, a path outside
-- the checkout, a repeat, past the prompt's byte guard), so what pruning
-- costs can be read later; dropped: how many were cut. answered: 1 when the
-- attempt's reviewer answered, so its leads were read; 0 for the runs of an
-- attempt whose reviewer didn't (out of form, failed, a model passed to the
-- next): they cost what they cost, but their leads went to no one.
ALTER TABLE audit_helper_runs ADD COLUMN cut TEXT NOT NULL DEFAULT '[]';
ALTER TABLE audit_helper_runs ADD COLUMN answered INTEGER NOT NULL DEFAULT 1;

-- samples: how many of the decision model's asks about the round answered (it
-- is asked 3 times at once, then in twos to 9 while the decision isn't
-- settled: any ask alone deciding differently from their mean after the
-- first three, the mean's decision changing with the asks just added after
-- that; a failed ask is asked again while others answer): the scores are
-- their mean. spread: for each question their calls split on, every ask's
-- call, as JSON ({"f2":["serious","moderate",…]}); under "outcome" what each
-- ask alone would decide, when they differ; under "unsettled" the mean's
-- decision after each step, when it still changed at the last; '{}' when
-- they agreed. samples 0: no ask answered (or the round never got as far as
-- asking).
ALTER TABLE review_again_decisions ADD COLUMN samples INTEGER NOT NULL DEFAULT 0;
ALTER TABLE review_again_decisions ADD COLUMN spread TEXT NOT NULL DEFAULT '{}';
