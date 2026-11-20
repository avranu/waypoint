-- Files on anything (WP-D16): attachments belong to a target (an issue, page,
-- design note, comment, request or session) instead of only an issue. A
-- target's id isn't a foreign key (requests and sessions live in tables other
-- slices own); deleting a page, design note or comment removes its files
-- through the triggers below. issue_id stays for the targets that live on an
-- issue (the issue itself and its comments) so deleting the issue cascades.

DROP INDEX attachments_issue;
ALTER TABLE attachments RENAME TO attachments_old;

CREATE TABLE attachments (
    -- AUTOINCREMENT: ids are never reused, since file URLs are cached forever.
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    target_type  TEXT    NOT NULL, -- issue, page, design, comment, request, session
    target_id    INTEGER NOT NULL,
    issue_id     INTEGER REFERENCES issues(id) ON DELETE CASCADE, -- issue and comment targets
    uploader_id  INTEGER REFERENCES users(id) ON DELETE SET NULL,
    agent        TEXT    NOT NULL DEFAULT '', -- the agent that uploaded it, if one did
    name         TEXT    NOT NULL,
    content_type TEXT    NOT NULL,
    size         INTEGER NOT NULL,
    sha256       TEXT    NOT NULL,
    caption      TEXT    NOT NULL DEFAULT '',
    data         BLOB    NOT NULL,
    created_at   INTEGER NOT NULL
);
CREATE INDEX attachments_target ON attachments(target_type, target_id, id);
CREATE INDEX attachments_issue ON attachments(issue_id) WHERE issue_id IS NOT NULL;

INSERT INTO attachments(id, target_type, target_id, issue_id, uploader_id, name, content_type, size, sha256, data, created_at)
    SELECT id, 'issue', issue_id, issue_id, uploader_id, name, content_type, size, sha256, data, created_at
    FROM attachments_old;

-- Keep the id counter, so a deleted file's id is never handed out again.
DELETE FROM sqlite_sequence WHERE name = 'attachments';
UPDATE sqlite_sequence SET name = 'attachments' WHERE name = 'attachments_old';
DROP TABLE attachments_old;

CREATE TRIGGER attachments_page_deleted AFTER DELETE ON pages BEGIN
    DELETE FROM attachments WHERE target_type = 'page' AND target_id = old.id;
END;
CREATE TRIGGER attachments_design_deleted AFTER DELETE ON design_notes BEGIN
    DELETE FROM attachments WHERE target_type = 'design' AND target_id = old.id;
END;
CREATE TRIGGER attachments_comment_deleted AFTER DELETE ON comments BEGIN
    DELETE FROM attachments WHERE target_type = 'comment' AND target_id = old.id;
END;
