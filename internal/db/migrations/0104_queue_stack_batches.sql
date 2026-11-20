-- Merge queue batches (WP-509): while the CI runners are saturated, the
-- front of a merge queue and the next few pull requests are tested in one
-- run, on one wp-queue/ branch, and merge on it in order. Each pull request
-- that rides the batch has a row of its own, all with the same heads,
-- branch and commit; pos is the index in heads of the row's own pull
-- request's head. -1: the last (a stack of its own, as before).
ALTER TABLE queue_stacks ADD COLUMN pos INTEGER NOT NULL DEFAULT -1;
