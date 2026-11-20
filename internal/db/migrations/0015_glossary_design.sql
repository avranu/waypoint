-- Glossary, design notes, links between everything, comment search and
-- when an agent's claim began (ADR 0010).

-- A term, per project or global (project_id NULL).
CREATE TABLE glossary (
    id            INTEGER PRIMARY KEY,
    project_id    INTEGER REFERENCES projects(id) ON DELETE CASCADE,
    term          TEXT    NOT NULL,
    aliases       TEXT    NOT NULL DEFAULT '[]', -- JSON array of strings
    definition    TEXT    NOT NULL DEFAULT '',
    locked_by     INTEGER REFERENCES users(id) ON DELETE SET NULL,
    locked_at     INTEGER, -- set: agents can't change or delete the entry
    created_by    INTEGER REFERENCES users(id) ON DELETE SET NULL,
    created_agent TEXT    NOT NULL DEFAULT '',
    created_at    INTEGER NOT NULL,
    updated_by    INTEGER REFERENCES users(id) ON DELETE SET NULL,
    updated_agent TEXT    NOT NULL DEFAULT '',
    updated_at    INTEGER NOT NULL
);
CREATE UNIQUE INDEX glossary_term ON glossary(coalesce(project_id, 0), term COLLATE NOCASE);

-- Every saved state of an entry, for the audit trail.
CREATE TABLE glossary_revisions (
    id         INTEGER PRIMARY KEY,
    entry_id   INTEGER NOT NULL REFERENCES glossary(id) ON DELETE CASCADE,
    term       TEXT    NOT NULL,
    aliases    TEXT    NOT NULL DEFAULT '[]',
    definition TEXT    NOT NULL DEFAULT '',
    locked     INTEGER NOT NULL DEFAULT 0,
    user_id    INTEGER REFERENCES users(id) ON DELETE SET NULL,
    agent      TEXT    NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL
);
CREATE INDEX glossary_revisions_entry ON glossary_revisions(entry_id, id);

CREATE VIRTUAL TABLE glossary_search USING fts5(term, aliases, definition); -- rowid = glossary.id

-- What people intend: goals, features, screens, decisions… Approved by a
-- person (approved_at) or proposed by an agent (approved_at NULL).
CREATE TABLE design_notes (
    id            INTEGER PRIMARY KEY,
    project_id    INTEGER REFERENCES projects(id) ON DELETE CASCADE, -- NULL: global
    parent_id     INTEGER REFERENCES design_notes(id) ON DELETE SET NULL,
    kind          TEXT    NOT NULL DEFAULT 'note',
    title         TEXT    NOT NULL,
    body          TEXT    NOT NULL DEFAULT '',
    status        TEXT    NOT NULL DEFAULT 'active', -- active, superseded, retired
    approved_by   INTEGER REFERENCES users(id) ON DELETE SET NULL,
    approved_at   INTEGER,
    created_by    INTEGER REFERENCES users(id) ON DELETE SET NULL,
    created_agent TEXT    NOT NULL DEFAULT '',
    created_at    INTEGER NOT NULL,
    updated_by    INTEGER REFERENCES users(id) ON DELETE SET NULL,
    updated_agent TEXT    NOT NULL DEFAULT '',
    updated_at    INTEGER NOT NULL
);
CREATE INDEX design_notes_project ON design_notes(project_id);
CREATE INDEX design_notes_parent ON design_notes(parent_id);

-- Edits (title/body snapshots) and additions (appended text) to a note.
-- People's are approved when saved; agents' wait for a person once the
-- note is approved.
CREATE TABLE design_revisions (
    id          INTEGER PRIMARY KEY,
    note_id     INTEGER NOT NULL REFERENCES design_notes(id) ON DELETE CASCADE,
    kind        TEXT    NOT NULL, -- edit, addition
    title       TEXT    NOT NULL DEFAULT '',
    body        TEXT    NOT NULL DEFAULT '',
    base_id     INTEGER, -- edits: the approved revision it started from
    state       TEXT    NOT NULL, -- approved, pending, rejected, merged
    user_id     INTEGER REFERENCES users(id) ON DELETE SET NULL,
    agent       TEXT    NOT NULL DEFAULT '',
    created_at  INTEGER NOT NULL,
    reviewed_by INTEGER REFERENCES users(id) ON DELETE SET NULL,
    reviewed_at INTEGER,
    review_note TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX design_revisions_note ON design_revisions(note_id, id);
CREATE INDEX design_revisions_pending ON design_revisions(note_id) WHERE state = 'pending';

CREATE VIRTUAL TABLE design_search USING fts5(title, body); -- rowid = design_notes.id; body includes additions

-- Links between issues, pages, glossary entries and design notes, and
-- from them to files and URLs. auto: made from a [[wikilink]] in the
-- source's text (replaced whenever that text is saved).
CREATE TABLE knowledge_links (
    id            INTEGER PRIMARY KEY,
    src_type      TEXT    NOT NULL, -- issue, page, glossary, design
    src_id        INTEGER NOT NULL,
    dst_type      TEXT    NOT NULL, -- issue, page, glossary, design, file, url
    dst_id        INTEGER,          -- issue, page, glossary, design
    dst_ref       TEXT    NOT NULL DEFAULT '', -- file path or URL
    rel           TEXT    NOT NULL DEFAULT 'relates',
    note          TEXT    NOT NULL DEFAULT '',
    auto          INTEGER NOT NULL DEFAULT 0,
    created_by    INTEGER REFERENCES users(id) ON DELETE SET NULL,
    created_agent TEXT    NOT NULL DEFAULT '',
    created_at    INTEGER NOT NULL
);
CREATE UNIQUE INDEX knowledge_links_unique ON knowledge_links(src_type, src_id, dst_type, coalesce(dst_id, 0), dst_ref, rel);
CREATE INDEX knowledge_links_dst ON knowledge_links(dst_type, dst_id);

CREATE TRIGGER knowledge_links_issue AFTER DELETE ON issues BEGIN
    DELETE FROM knowledge_links WHERE (src_type = 'issue' AND src_id = old.id) OR (dst_type = 'issue' AND dst_id = old.id);
END;
CREATE TRIGGER knowledge_links_page AFTER DELETE ON pages BEGIN
    DELETE FROM knowledge_links WHERE (src_type = 'page' AND src_id = old.id) OR (dst_type = 'page' AND dst_id = old.id);
END;
CREATE TRIGGER knowledge_links_glossary AFTER DELETE ON glossary BEGIN
    DELETE FROM knowledge_links WHERE (src_type = 'glossary' AND src_id = old.id) OR (dst_type = 'glossary' AND dst_id = old.id);
    DELETE FROM glossary_search WHERE rowid = old.id;
END;
CREATE TRIGGER knowledge_links_design AFTER DELETE ON design_notes BEGIN
    DELETE FROM knowledge_links WHERE (src_type = 'design' AND src_id = old.id) OR (dst_type = 'design' AND dst_id = old.id);
    DELETE FROM design_search WHERE rowid = old.id;
END;

-- Comments on their own, so search can point at the comment that matched.
CREATE VIRTUAL TABLE comment_search USING fts5(body); -- rowid = comments.id
INSERT INTO comment_search(rowid, body) SELECT id, body FROM comments;
CREATE TRIGGER comment_search_insert AFTER INSERT ON comments BEGIN
    INSERT INTO comment_search(rowid, body) VALUES (new.id, new.body);
END;
CREATE TRIGGER comment_search_update AFTER UPDATE OF body ON comments BEGIN
    DELETE FROM comment_search WHERE rowid = old.id;
    INSERT INTO comment_search(rowid, body) VALUES (new.id, new.body);
END;
CREATE TRIGGER comment_search_delete AFTER DELETE ON comments BEGIN
    DELETE FROM comment_search WHERE rowid = old.id;
END;

-- When the current claim began, for the agent's running timer.
ALTER TABLE issues ADD COLUMN claimed_at INTEGER;
UPDATE issues SET claimed_at = (SELECT max(r.started_at) FROM agent_runs r
    WHERE r.issue_id = issues.id AND r.agent = issues.claimed_by AND r.ended_at IS NULL)
    WHERE claimed_by != '';
