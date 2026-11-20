-- WP-245: screenshots an agent took for the person reviewing its work
-- (wp shot), shown on the Catch up page ahead of what it merely looked at.
ALTER TABLE attachments ADD COLUMN review INTEGER NOT NULL DEFAULT 0;

-- Where a person can try a finished task (WP-245): a page that shows the
-- change, recorded by wp shot (or wp done --shot) with the screenshots of
-- it. url is absolute, or a path ("/live") that the project's production
-- (else staging) environment URL completes, for pages shot on a local
-- server.
CREATE TABLE try_links (
    id         INTEGER PRIMARY KEY,
    issue_id   INTEGER NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
    url        TEXT    NOT NULL,
    user_id    INTEGER REFERENCES users(id) ON DELETE SET NULL,
    agent      TEXT    NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,
    UNIQUE (issue_id, url)
);
