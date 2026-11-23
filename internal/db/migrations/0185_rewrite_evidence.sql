-- What wp ship saw when a change replaced lines another task's pull request
-- put on the base branch (WP-1370, WP-782): the pair, the pull request, the
-- lines and their share, and the head commit it was computed for. The
-- review round reads it as a lead for its check of earlier work undone, so
-- the agent isn't asked to relate tasks Waypoint already related.
--
-- One row per pair, the newest ship's reading. The relation it explains is
-- the inferred 'relates' with found 'rewrite' in issue_relations: once an
-- agent or a person replaces that relation the row no longer counts.
CREATE TABLE rewrite_evidence (
    issue_id   INTEGER NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
    other_id   INTEGER NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
    repo       TEXT    NOT NULL,
    pr         INTEGER NOT NULL,           -- the other task's pull request with most lines replaced
    prs        INTEGER NOT NULL DEFAULT 1, -- how many of its pull requests the lines are from
    lines      INTEGER NOT NULL,
    share      REAL    NOT NULL,           -- of the lines the change replaces
    merged_at  INTEGER NOT NULL,           -- ms, when pr reached the base
    head       TEXT    NOT NULL DEFAULT '',
    note       TEXT    NOT NULL,
    dismissed  INTEGER NOT NULL DEFAULT 0, -- 1: a person removed the relation: it is not made again
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    PRIMARY KEY (issue_id, other_id)
);
CREATE INDEX rewrite_evidence_other ON rewrite_evidence(other_id);
