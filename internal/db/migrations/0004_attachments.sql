-- Files attached to issues. Kept in the database (as one row each, loaded
-- only when downloaded) so a backup is still a single file.

CREATE TABLE attachments (
    -- AUTOINCREMENT: ids are never reused, since file URLs are cached forever.
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    issue_id     INTEGER NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
    uploader_id  INTEGER REFERENCES users(id) ON DELETE SET NULL,
    name         TEXT    NOT NULL,
    content_type TEXT    NOT NULL,
    size         INTEGER NOT NULL,
    sha256       TEXT    NOT NULL,
    data         BLOB    NOT NULL,
    created_at   INTEGER NOT NULL
);
CREATE INDEX attachments_issue ON attachments(issue_id);
