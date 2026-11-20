-- Forge mentions (WP-379): every place a pull request, commit or forge issue
-- (or a comment on one) names an issue key, kept as evidence of a
-- relationship. Only the item that is the issue's own work (named in its
-- branch or title, or with "Fixes", "Closes" and the like) becomes an
-- external link, whose effects (In Review, wp done waiting, requests closing,
-- automations) belong to that work; a key mentioned in passing ("follow-up to
-- WP-12", a list of related tasks in a commit body) is only recorded here.
--
-- One row per issue, piece of text (source_url: the item, or the comment
-- on it) and place in that text. gone_at: the text no longer names it (an
-- edit or a deleted comment), kept so it is known it once did.
CREATE TABLE forge_mentions (
    id          INTEGER PRIMARY KEY,
    issue_id    INTEGER NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
    forge_id    INTEGER NOT NULL REFERENCES forges(id) ON DELETE CASCADE,
    repo        TEXT    NOT NULL,
    item_kind   TEXT    NOT NULL,              -- pull, commit, issue
    item_url    TEXT    NOT NULL,              -- the pull request, commit or forge issue
    number      INTEGER,                       -- its number (not for commits)
    source_url  TEXT    NOT NULL,              -- where the text is: item_url, or a comment's URL
    place       TEXT    NOT NULL,              -- title, branch, body, subject, message, comment, review
    verb        TEXT    NOT NULL DEFAULT '',   -- the relationship the text states: fixes, closes, part_of, follows, relates, reverts, ... ('' none)
    own         INTEGER NOT NULL DEFAULT 0,    -- 1: this mention makes the item the issue's own work (linked)
    excerpt     TEXT    NOT NULL DEFAULT '',   -- the line naming it ('' for text from outside: WP-279)
    title       TEXT    NOT NULL DEFAULT '',   -- the item's title as a link shows it ('' for outside text)
    state       TEXT    NOT NULL DEFAULT '',   -- the item's state when last seen
    author      TEXT    NOT NULL DEFAULT '',   -- the forge login who wrote the text
    created_at  INTEGER NOT NULL,
    updated_at  INTEGER NOT NULL,
    gone_at     INTEGER,
    UNIQUE (issue_id, source_url, place)
);
CREATE INDEX forge_mentions_item ON forge_mentions(item_url);
CREATE INDEX forge_mentions_source ON forge_mentions(source_url);

-- How a forge link was made: 'added' (by hand or wp ship), or the place in
-- the item's text that made it (title, branch, body, subject). '' for links
-- made before WP-379.
ALTER TABLE external_links ADD COLUMN via TEXT NOT NULL DEFAULT '';

-- A forge item someone unlinked from an issue: the item's text is scanned
-- again on every change, and must not link it back. Adding the link again
-- clears it.
CREATE TABLE issue_unlinks (
    issue_id    INTEGER NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
    url         TEXT    NOT NULL,
    user_id     INTEGER REFERENCES users(id) ON DELETE SET NULL,
    agent       TEXT    NOT NULL DEFAULT '',
    created_at  INTEGER NOT NULL,
    PRIMARY KEY (issue_id, url)
);
