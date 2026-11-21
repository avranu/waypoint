-- Rows an agent writes say which session wrote them (WP-ADR145 point 6), and
-- the task that session held at that moment, on the row itself rather than
-- only in events.data.session. NULL is unknown: a row written by a person, by
-- a client that sent no session, or before this migration. Nothing is guessed.
-- Retention: nothing prunes events, knowledge_uses or knowledge_shown by age
-- (the hourly expiry job in scheduler.go clears only logins, sessions, links
-- and notifications); rows leave with their item, issue or project. So the
-- event-log backfill below sees the whole history, and shown rows grow
-- without bound until a measured size says otherwise.
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
-- the extra reads it stood for, so reads are not hidden by the dedupe. NULL
-- on a row written before this: repeats then were dropped unrecorded, so the
-- count is unknown, not 0 (WP-ADR145 point 4).
ALTER TABLE knowledge_uses ADD COLUMN repeats INTEGER;

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

-- Backfill only what the event log states exactly, marked so a report can
-- leave these out. Two tables qualify:
--  * comments: issue.commented events carry the comment id and the session.
--    The task the session held then is not stated (it is recorded only when it
--    differs from the commented task), so session_issue_id stays NULL.
--  * issues: the issue.created event is on the new issue and carries the
--    session (same caveat for the task).
-- Not backfilled, because the log does not name the row: issue_relations and
-- knowledge_links (their events carry no row id, so a match would be a guess),
-- knowledge_uses, design_revisions and pages (no event names the row's
-- session). They stay NULL.
-- An event is matched on its issue as well as the comment id (the issue index
-- keeps this to each issue's own events), and only when it is no older than
-- the comment: comments.id is reused after the newest is deleted, and the
-- deleted comment's event stays in the log.
ALTER TABLE comments ADD COLUMN session_backfilled INTEGER NOT NULL DEFAULT 0;
UPDATE comments SET session_id = m.sid, session_backfilled = 1
FROM (
    SELECT cid, sid FROM (
        SELECT c.id AS cid, json_extract(e.data, '$.session') AS sid,
               row_number() OVER (PARTITION BY c.id ORDER BY e.id) AS rn
        FROM comments c
        JOIN events e ON e.issue_id = c.issue_id AND e.kind = 'issue.commented'
        WHERE c.session_id IS NULL AND e.created_at >= c.created_at
          AND json_extract(e.data, '$.comment_id') = c.id
          AND json_type(e.data, '$.session') = 'integer'
          AND EXISTS (SELECT 1 FROM agent_sessions s WHERE s.id = json_extract(e.data, '$.session')))
    WHERE rn = 1) m
WHERE comments.id = m.cid;

-- The same age guard for issues: deleting an issue cascades to its events,
-- but a row left from a time foreign keys were off must not hand a reused id
-- the deleted issue's session.
ALTER TABLE issues ADD COLUMN session_backfilled INTEGER NOT NULL DEFAULT 0;
UPDATE issues SET session_id = m.sid, session_backfilled = 1
FROM (
    SELECT iid, sid FROM (
        SELECT i.id AS iid, json_extract(e.data, '$.session') AS sid,
               row_number() OVER (PARTITION BY i.id ORDER BY e.id) AS rn
        FROM issues i
        JOIN events e ON e.issue_id = i.id AND e.kind = 'issue.created'
        WHERE i.session_id IS NULL AND e.created_at >= i.created_at
          AND json_type(e.data, '$.session') = 'integer'
          AND EXISTS (SELECT 1 FROM agent_sessions s WHERE s.id = json_extract(e.data, '$.session')))
    WHERE rn = 1) m
WHERE issues.id = m.iid;
