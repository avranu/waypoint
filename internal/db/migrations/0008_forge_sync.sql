-- waypoint:no-foreign-keys
-- GitLab forges, auto-created projects per repo, and two-way comment sync.

-- forges.kind gains 'gitlab'; SQLite can't alter a CHECK, so rebuild.
CREATE TABLE forges_new (
    id             INTEGER PRIMARY KEY,
    kind           TEXT    NOT NULL CHECK (kind IN ('github','forgejo','gitlab')),
    name           TEXT    NOT NULL,
    base_url       TEXT    NOT NULL,
    token          TEXT    NOT NULL DEFAULT '',
    webhook_secret TEXT    NOT NULL DEFAULT '',
    auto_projects  INTEGER NOT NULL DEFAULT 0, -- make a project for every repo it reports
    created_by     INTEGER REFERENCES users(id) ON DELETE SET NULL,
    created_at     INTEGER NOT NULL
);
INSERT INTO forges_new(id, kind, name, base_url, token, webhook_secret, created_at)
    SELECT id, kind, name, base_url, token, webhook_secret, created_at FROM forges;
DROP TABLE forges;
ALTER TABLE forges_new RENAME TO forges;

-- The project standing for an organisation or group, shared by every forge
-- (a GitHub mirror of a Forgejo org lands in the same place).
CREATE TABLE forge_namespaces (
    namespace  TEXT    PRIMARY KEY COLLATE NOCASE, -- "acme", or "group/subgroup"
    project_id INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE
);

-- Who wrote a comment that came from a forge.
ALTER TABLE comments ADD COLUMN external_author TEXT NOT NULL DEFAULT '';
ALTER TABLE comments ADD COLUMN external_via    TEXT NOT NULL DEFAULT ''; -- GitHub, Forgejo, GitLab

-- A Waypoint comment and its copy on a forge, so nothing is mirrored twice.
CREATE TABLE comment_links (
    comment_id  INTEGER NOT NULL REFERENCES comments(id) ON DELETE CASCADE,
    forge_id    INTEGER NOT NULL REFERENCES forges(id) ON DELETE CASCADE,
    external_id TEXT    NOT NULL, -- the forge's comment id
    url         TEXT    NOT NULL DEFAULT '',
    origin      INTEGER NOT NULL DEFAULT 0, -- 1: the comment came from this forge; 0: Waypoint's copy
    PRIMARY KEY (forge_id, external_id)
);
CREATE INDEX comment_links_comment ON comment_links(comment_id);

-- Whether Waypoint comments are posted to this repo's issues. Repos joined to
-- a project automatically because they share a name with one already there
-- (a mirror) start off: a public mirror must not receive a private repo's
-- discussion unless someone says so.
ALTER TABLE project_repos ADD COLUMN post_comments INTEGER NOT NULL DEFAULT 1;
