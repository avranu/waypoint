-- Speculative merge queue (WP-404, WP-ADR33): the pull requests at places 2
-- and 3 of a merge queue are tested on their base's tip with every head ahead
-- of them merged in, on a branch Waypoint makes (wp-queue/...) whose push run
-- builds those merges. One row per such stack.
--
-- heads: JSON [{"pr_id":…,"number":…,"sha":…}], front first; the last is the
-- pull request the stack is for. base_sha: the base's tip the stack's branch
-- was cut from. state: running, passed, failed, ready (its pull request was
-- marked ready on it), merged, cancelled, refused. tree: the tree the run
-- built, read from its log ('' until known).
CREATE TABLE queue_stacks (
    id          INTEGER PRIMARY KEY,
    forge_id    INTEGER NOT NULL REFERENCES forges(id) ON DELETE CASCADE,
    repo        TEXT    NOT NULL,
    base_ref    TEXT    NOT NULL,
    pr_id       INTEGER NOT NULL REFERENCES pull_requests(id) ON DELETE CASCADE,
    base_sha    TEXT    NOT NULL,
    heads       TEXT    NOT NULL DEFAULT '[]',
    branch      TEXT    NOT NULL,
    commit_sha  TEXT    NOT NULL DEFAULT '',
    state       TEXT    NOT NULL DEFAULT 'running',
    tree        TEXT    NOT NULL DEFAULT '',
    run_url     TEXT    NOT NULL DEFAULT '',
    note        TEXT    NOT NULL DEFAULT '',
    created_at  INTEGER NOT NULL,
    ended_at    INTEGER
);
CREATE INDEX queue_stacks_pr ON queue_stacks(pr_id, ended_at);
CREATE INDEX queue_stacks_live ON queue_stacks(forge_id, repo, base_ref, ended_at);
CREATE INDEX queue_stacks_commit ON queue_stacks(commit_sha);
