-- waypoint:no-foreign-keys
-- Forge sync fixes: mirror links apart from mentions, one forge comment on
-- several issues, deleted comments that stay deleted, repos an admin
-- detached, and no comment posting for repos attached before 0008.

-- An issue link is a mirror (the Waypoint issue *is* that forge issue: its
-- text, state and comments sync) or a mention (a forge issue that names
-- the Waypoint issue: linked, nothing synced). Before this, mirrors were
-- only made by imports, which create issues as the system (no reporter).
ALTER TABLE external_links ADD COLUMN mirror INTEGER NOT NULL DEFAULT 0;
UPDATE external_links SET mirror = 1 WHERE kind = 'issue'
    AND issue_id IN (SELECT id FROM issues WHERE reporter_id IS NULL);

-- A forge comment is copied onto every issue mirroring its forge issue
-- (the same repo attached to several projects), so it can have several
-- Waypoint copies.
CREATE TABLE comment_links_new (
    comment_id  INTEGER NOT NULL REFERENCES comments(id) ON DELETE CASCADE,
    forge_id    INTEGER NOT NULL REFERENCES forges(id) ON DELETE CASCADE,
    external_id TEXT    NOT NULL, -- the forge's comment id
    url         TEXT    NOT NULL DEFAULT '',
    origin      INTEGER NOT NULL DEFAULT 0, -- 1: the comment came from this forge; 0: Waypoint's copy
    PRIMARY KEY (forge_id, external_id, comment_id)
);
INSERT INTO comment_links_new(comment_id, forge_id, external_id, url, origin)
    SELECT comment_id, forge_id, external_id, url, origin FROM comment_links;
DROP TABLE comment_links;
ALTER TABLE comment_links_new RENAME TO comment_links;
CREATE INDEX comment_links_comment ON comment_links(comment_id);

-- Forge comments whose Waypoint copy was deleted on an issue: a later sync
-- must not bring them back.
CREATE TABLE comment_tombstones (
    issue_id    INTEGER NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
    forge_id    INTEGER NOT NULL REFERENCES forges(id) ON DELETE CASCADE,
    external_id TEXT    NOT NULL,
    deleted_at  INTEGER NOT NULL,
    PRIMARY KEY (issue_id, forge_id, external_id)
);

-- Repos an admin detached: automatic projects don't re-attach them.
CREATE TABLE forge_repo_ignores (
    forge_id   INTEGER NOT NULL REFERENCES forges(id) ON DELETE CASCADE,
    repo       TEXT    NOT NULL COLLATE NOCASE,
    created_at INTEGER NOT NULL,
    PRIMARY KEY (forge_id, repo)
);

-- 0008 added post_comments defaulting to on, which turned on posting for
-- repos attached before Waypoint could post comments at all. Those start
-- off; repos attached since 0008 keep what they were given.
UPDATE project_repos SET post_comments = 0
    WHERE created_at < (SELECT applied_at * 1000 FROM schema_migrations WHERE name = '0008_forge_sync.sql');
