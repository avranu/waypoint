-- Text from outside (WP-279, WP-ADR11): forge issues and comments written by
-- someone who isn't a Waypoint person never reach an agent or a model until
-- a person approves them. See core/outside.go.

-- Forge logins that are Waypoint people: their text carries their own
-- authority and syncs as before. An admin maps them on the forge (and the
-- login of the forge's own token is mapped to whoever connected it).
CREATE TABLE forge_people (
    forge_id INTEGER NOT NULL REFERENCES forges(id) ON DELETE CASCADE,
    login    TEXT    NOT NULL COLLATE NOCASE,
    user_id  INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    PRIMARY KEY (forge_id, login)
);
-- 1 once the token's own login has been mapped (or tried): an admin who
-- removes that mapping isn't overruled.
ALTER TABLE forges ADD COLUMN people_seeded INTEGER NOT NULL DEFAULT 0;

-- An issue imported from outside is a draft (WP-42) until a person approves
-- it: outside = 1, with who wrote it (outside_by, a forge login; '' when
-- not known yet) and on which forge.
ALTER TABLE issues ADD COLUMN outside INTEGER NOT NULL DEFAULT 0;
ALTER TABLE issues ADD COLUMN outside_by TEXT NOT NULL DEFAULT '';
ALTER TABLE issues ADD COLUMN outside_forge INTEGER REFERENCES forges(id) ON DELETE SET NULL;

-- Outside text waiting for a person, kept apart from issues and comments so
-- that nothing reading those can see it: a new comment (comment_id NULL), a
-- new text for a comment (comment_id set), or a new title and description
-- for an issue (external_id ''). A person who discards it keeps it out:
-- discarded = 1 until the forge sends a different text.
CREATE TABLE outside_text (
    id          INTEGER PRIMARY KEY,
    issue_id    INTEGER NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
    comment_id  INTEGER REFERENCES comments(id) ON DELETE CASCADE,
    forge_id    INTEGER NOT NULL REFERENCES forges(id) ON DELETE CASCADE,
    external_id TEXT    NOT NULL DEFAULT '',
    url         TEXT    NOT NULL DEFAULT '',
    author      TEXT    NOT NULL DEFAULT '',
    title       TEXT    NOT NULL DEFAULT '',
    body        TEXT    NOT NULL DEFAULT '',
    created_at  INTEGER NOT NULL,
    updated_at  INTEGER NOT NULL,
    discarded   INTEGER NOT NULL DEFAULT 0,
    UNIQUE (issue_id, forge_id, external_id)
);
CREATE INDEX outside_text_author ON outside_text(forge_id, author COLLATE NOCASE);

-- What was imported before this: who wrote it isn't known yet, so it waits
-- like any outside text. The sync starts over and reads every forge issue
-- and comment again with its author; a Waypoint person's text goes back at
-- once.
UPDATE issues SET draft = 1, outside = 1
WHERE draft = 0 AND id IN (SELECT issue_id FROM external_links WHERE mirror = 1);
UPDATE issues SET outside_forge = (
    SELECT r.forge_id FROM external_links el
    JOIN project_repos r ON r.project_id = issues.project_id AND lower(r.repo) = lower(el.repo)
    WHERE el.issue_id = issues.id AND el.mirror = 1 LIMIT 1)
WHERE outside = 1;

CREATE TEMP TABLE outside_moved AS
SELECT c.id AS comment_id, c.issue_id FROM comments c JOIN comment_links l ON l.comment_id = c.id AND l.origin = 1;
INSERT OR IGNORE INTO outside_text(issue_id, forge_id, external_id, url, author, body, created_at, updated_at)
SELECT c.issue_id, l.forge_id, l.external_id, l.url, c.external_author, c.body, c.created_at, c.updated_at
FROM comments c JOIN comment_links l ON l.comment_id = c.id AND l.origin = 1;
UPDATE events SET new_value = '' WHERE kind = 'issue.commented'
    AND json_extract(data, '$.comment_id') IN (SELECT comment_id FROM outside_moved);
DELETE FROM comments WHERE id IN (SELECT comment_id FROM outside_moved);
-- The issues' search text held those comments too.
DELETE FROM issue_search WHERE rowid IN (SELECT issue_id FROM outside_moved);
INSERT INTO issue_search(rowid, key, title, body)
SELECT i.id, p.key || '-' || i.number, i.title,
    i.description || ' ' || coalesce((SELECT group_concat(c.body, ' ') FROM comments c WHERE c.issue_id = i.id), '')
FROM issues i JOIN projects p ON p.id = i.project_id
WHERE i.id IN (SELECT DISTINCT issue_id FROM outside_moved);
DROP TABLE outside_moved;

-- Titles of forge issues linked from Waypoint issues come back only for
-- forge issues a Waypoint person wrote.
UPDATE external_links SET title = '' WHERE kind = 'issue' AND mirror = 0 AND provider IN ('github', 'forgejo', 'gitlab');

UPDATE project_repos SET last_synced_at = NULL WHERE sync_issues = 1;
