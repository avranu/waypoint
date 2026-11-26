-- Each change of what a babysit is doing and waiting for, with the time, so a
-- pull request's open time can be given to a stage afterwards (WP-1910): the
-- babysit row keeps only the latest state and wait.
CREATE TABLE babysit_waits (
    id         INTEGER PRIMARY KEY,
    pr_id      INTEGER NOT NULL REFERENCES pull_requests(id) ON DELETE CASCADE,
    babysit_id INTEGER NOT NULL,
    state      TEXT    NOT NULL,
    wait       TEXT    NOT NULL DEFAULT '',
    at         INTEGER NOT NULL
);
CREATE INDEX babysit_waits_pr ON babysit_waits(pr_id, at);

-- Each stay in the merge line: queued_at is cleared on leaving, so without
-- this a pull request that joined, left and rejoined shows only the last
-- join. why is the reason the code had when it left.
CREATE TABLE merge_line_spans (
    id        INTEGER PRIMARY KEY,
    pr_id     INTEGER NOT NULL REFERENCES pull_requests(id) ON DELETE CASCADE,
    joined_at INTEGER NOT NULL,
    left_at   INTEGER,
    why       TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX merge_line_spans_pr ON merge_line_spans(pr_id, joined_at);
