-- Rows an agent writes say which session wrote them (WP-ADR145 point 6), and
-- the task that session held at that moment, on the row itself rather than
-- only in events.data.session. NULL is unknown: a row written by a person, by
-- a client that sent no session, or before this migration. Nothing is guessed.
ALTER TABLE issue_relations ADD COLUMN session_id INTEGER REFERENCES agent_sessions(id) ON DELETE SET NULL;
ALTER TABLE issue_relations ADD COLUMN session_issue_id INTEGER REFERENCES issues(id) ON DELETE SET NULL;
ALTER TABLE knowledge_links ADD COLUMN session_id INTEGER REFERENCES agent_sessions(id) ON DELETE SET NULL;
ALTER TABLE knowledge_links ADD COLUMN session_issue_id INTEGER REFERENCES issues(id) ON DELETE SET NULL;
ALTER TABLE knowledge_uses ADD COLUMN session_id INTEGER REFERENCES agent_sessions(id) ON DELETE SET NULL;
ALTER TABLE knowledge_uses ADD COLUMN session_issue_id INTEGER REFERENCES issues(id) ON DELETE SET NULL;
ALTER TABLE comments ADD COLUMN session_id INTEGER REFERENCES agent_sessions(id) ON DELETE SET NULL;
ALTER TABLE comments ADD COLUMN session_issue_id INTEGER REFERENCES issues(id) ON DELETE SET NULL;
ALTER TABLE design_revisions ADD COLUMN session_id INTEGER REFERENCES agent_sessions(id) ON DELETE SET NULL;
ALTER TABLE design_revisions ADD COLUMN session_issue_id INTEGER REFERENCES issues(id) ON DELETE SET NULL;
ALTER TABLE issues ADD COLUMN session_id INTEGER REFERENCES agent_sessions(id) ON DELETE SET NULL;
ALTER TABLE issues ADD COLUMN session_issue_id INTEGER REFERENCES issues(id) ON DELETE SET NULL;
-- pages already has session_id (0164, the session that created the page).
ALTER TABLE pages ADD COLUMN session_issue_id INTEGER REFERENCES issues(id) ON DELETE SET NULL;

-- A lookup repeated inside the 10-minute window is one row; repeats counts
-- the extra reads it stood for, so reads are not hidden by the dedupe.
ALTER TABLE knowledge_uses ADD COLUMN repeats INTEGER NOT NULL DEFAULT 0;

-- Each time a glossary entry, page or design note was put in front of an
-- agent without its asking (wp brief, a task's prep), against the lookups in
-- knowledge_uses: the denominator for "shown but never read". One row per
-- showing, no dedupe.
CREATE TABLE knowledge_shown (
    id               INTEGER PRIMARY KEY,
    item_type        TEXT    NOT NULL, -- glossary, page, design
    item_id          INTEGER NOT NULL,
    via              TEXT    NOT NULL, -- brief, prep
    session_id       INTEGER REFERENCES agent_sessions(id) ON DELETE SET NULL,
    session_issue_id INTEGER REFERENCES issues(id) ON DELETE SET NULL,
    project_id       INTEGER REFERENCES projects(id) ON DELETE CASCADE,
    user_id          INTEGER REFERENCES users(id) ON DELETE SET NULL,
    agent            TEXT    NOT NULL DEFAULT '',
    created_at       INTEGER NOT NULL
);
CREATE INDEX knowledge_shown_item ON knowledge_shown(item_type, item_id, created_at);
CREATE INDEX knowledge_shown_session ON knowledge_shown(session_id) WHERE session_id IS NOT NULL;
CREATE TRIGGER knowledge_shown_glossary AFTER DELETE ON glossary BEGIN
    DELETE FROM knowledge_shown WHERE item_type = 'glossary' AND item_id = old.id;
END;
CREATE TRIGGER knowledge_shown_page AFTER DELETE ON pages BEGIN
    DELETE FROM knowledge_shown WHERE item_type = 'page' AND item_id = old.id;
END;
CREATE TRIGGER knowledge_shown_design AFTER DELETE ON design_notes BEGIN
    DELETE FROM knowledge_shown WHERE item_type = 'design' AND item_id = old.id;
END;

-- Backfill what the event log states exactly: issue.commented events carry the
-- comment id and the session. The task the session held then is not stated
-- (it is only recorded when it differs from the commented task), so
-- session_issue_id stays NULL. Marked, so a report can leave these out.
ALTER TABLE comments ADD COLUMN session_backfilled INTEGER NOT NULL DEFAULT 0;
UPDATE comments SET session_id = (
        SELECT json_extract(e.data, '$.session') FROM events e
        WHERE e.kind = 'issue.commented' AND json_extract(e.data, '$.comment_id') = comments.id
          AND json_type(e.data, '$.session') = 'integer'
          AND EXISTS (SELECT 1 FROM agent_sessions s WHERE s.id = json_extract(e.data, '$.session'))
        ORDER BY e.id LIMIT 1),
    session_backfilled = 1
WHERE session_id IS NULL AND EXISTS (
        SELECT 1 FROM events e
        WHERE e.kind = 'issue.commented' AND json_extract(e.data, '$.comment_id') = comments.id
          AND json_type(e.data, '$.session') = 'integer'
          AND EXISTS (SELECT 1 FROM agent_sessions s WHERE s.id = json_extract(e.data, '$.session')));
