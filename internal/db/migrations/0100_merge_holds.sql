-- What held an armed pull request back, and for how long (WP-491): each
-- reason its merge check gave, with the times stripped, from the check that
-- first gave it to the first that no longer did. wp ci doctor splits a
-- merge's time after green by them: a reviewer's status not yet posted, the
-- quiet period, a round at work, review comments not dealt with.
CREATE TABLE merge_holds (
    id         INTEGER PRIMARY KEY,
    pr_id      INTEGER NOT NULL REFERENCES pull_requests(id) ON DELETE CASCADE,
    head_sha   TEXT    NOT NULL,
    what       TEXT    NOT NULL,
    first_at   INTEGER NOT NULL,
    cleared_at INTEGER
);
CREATE INDEX merge_holds_pr ON merge_holds(pr_id, head_sha);
CREATE INDEX merge_holds_open ON merge_holds(pr_id) WHERE cleared_at IS NULL;
