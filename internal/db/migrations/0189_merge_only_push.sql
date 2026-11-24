-- WP-1808: a push that only merges the base in, cleanly, onto a reviewed head
-- carries that review (and its waits) to the new head.
--
-- reviewed_heads: one row per head a review finished on (carried = 0) or
-- that carried the review of the head before it (carried = 1). diff_hash is
-- the normalised hash of the pull request's diff (core/review_carry.go);
-- content_sha and content_at are the head whose diff last differed and when
-- it was first seen, which the review wait and the expected-reviewer wait
-- count from.
CREATE TABLE reviewed_heads (
    pr_id       INTEGER NOT NULL REFERENCES pull_requests(id) ON DELETE CASCADE,
    head_sha    TEXT    NOT NULL,
    diff_hash   TEXT    NOT NULL,
    summary     TEXT    NOT NULL DEFAULT '',
    content_sha TEXT    NOT NULL,
    content_at  INTEGER NOT NULL,
    carried     INTEGER NOT NULL DEFAULT 0,
    from_sha    TEXT    NOT NULL DEFAULT '',
    created_at  INTEGER NOT NULL,
    PRIMARY KEY (pr_id, head_sha)
);

-- carried_sha: the head that carried; while it is the head, the waits count
-- from carried_at (and posts on carried_from_sha count) instead of head_at.
ALTER TABLE pull_requests ADD COLUMN carried_sha TEXT NOT NULL DEFAULT '';
ALTER TABLE pull_requests ADD COLUMN carried_from_sha TEXT NOT NULL DEFAULT '';
ALTER TABLE pull_requests ADD COLUMN carried_at INTEGER;

-- WP-1732: comments Waypoint posted itself through the pr_comment action
-- (and the landed note), by the id the forge gave them, so they are never
-- read back as review comments. automation_id, head_sha and body also say
-- what was asked on which head, so the same request isn't repeated on a head.
CREATE TABLE own_comments (
    pr_id         INTEGER NOT NULL REFERENCES pull_requests(id) ON DELETE CASCADE,
    ext_id        TEXT    NOT NULL,
    automation_id INTEGER NOT NULL DEFAULT 0,
    head_sha      TEXT    NOT NULL DEFAULT '',
    body          TEXT    NOT NULL DEFAULT '',
    created_at    INTEGER NOT NULL,
    PRIMARY KEY (pr_id, ext_id)
);
CREATE INDEX own_comments_asked ON own_comments(pr_id, automation_id, head_sha);
