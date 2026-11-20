-- Agent control and observability (WP-D15): the order agents take work in
-- and who paused them, which model did a run, where pull requests went and
-- who approved them, when knowledge is used, and a log of what people
-- decided. (A project's own pause lives in projects.settings.)

-- The agent queue: issues a person put in order for agents. Agents take
-- these first, lowest first, then the rest in the automatic order.
ALTER TABLE issues ADD COLUMN agent_order REAL;
CREATE INDEX issues_agent_order ON issues(agent_order) WHERE agent_order IS NOT NULL;

-- A person stops their agents (every one, a family or one model, written as
-- on issues) taking new work; work they hold stays theirs.
CREATE TABLE agent_pauses (
    id         INTEGER PRIMARY KEY,
    user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    spec       TEXT    NOT NULL, -- any, claude, haiku 5.5, …
    note       TEXT    NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,
    UNIQUE (user_id, spec)
);

-- The model behind a run, when tooling reports it (Claude Code's transcript
-- names it; the identity may only say anthropic/claude-code).
ALTER TABLE agent_runs ADD COLUMN model TEXT NOT NULL DEFAULT '';
CREATE INDEX agent_runs_started ON agent_runs(started_at);
CREATE INDEX agent_runs_agent ON agent_runs(agent, started_at);

-- Which agent linked an issue to a URL ('' = a person, or a forge mention).
ALTER TABLE external_links ADD COLUMN created_agent TEXT NOT NULL DEFAULT '';

-- A pull request's life on its forge.
ALTER TABLE pull_requests ADD COLUMN opened_at     INTEGER; -- the forge's created_at
ALTER TABLE pull_requests ADD COLUMN merged_at     INTEGER;
ALTER TABLE pull_requests ADD COLUMN closed_at     INTEGER;
ALTER TABLE pull_requests ADD COLUMN merged_by     TEXT NOT NULL DEFAULT ''; -- forge login
ALTER TABLE pull_requests ADD COLUMN agent         TEXT NOT NULL DEFAULT ''; -- the coding agent its text names
ALTER TABLE pull_requests ADD COLUMN additions     INTEGER;
ALTER TABLE pull_requests ADD COLUMN deletions     INTEGER;
ALTER TABLE pull_requests ADD COLUMN changed_files INTEGER;
ALTER TABLE pull_requests ADD COLUMN commits       INTEGER;
CREATE INDEX pull_requests_updated ON pull_requests(updated_at);
UPDATE pull_requests SET opened_at = created_at;

-- What happened on a pull request, as its forge reported it.
CREATE TABLE pr_events (
    id         INTEGER PRIMARY KEY,
    pr_id      INTEGER NOT NULL REFERENCES pull_requests(id) ON DELETE CASCADE,
    kind       TEXT    NOT NULL, -- opened, pushed, ready, reopened, commented, reviewed, approved, changes_requested, merged, closed
    actor      TEXT    NOT NULL DEFAULT '', -- forge login
    bot        INTEGER NOT NULL DEFAULT 0,  -- the actor is a bot account
    sha        TEXT    NOT NULL DEFAULT '',
    url        TEXT    NOT NULL DEFAULT '',
    body       TEXT    NOT NULL DEFAULT '', -- an excerpt
    ext_id     TEXT    NOT NULL DEFAULT '', -- the forge's id for it, so a repeated delivery counts once
    created_at INTEGER NOT NULL
);
CREATE INDEX pr_events_pr ON pr_events(pr_id, created_at);
CREATE UNIQUE INDEX pr_events_once ON pr_events(pr_id, kind, ext_id) WHERE ext_id != '';
INSERT INTO pr_events(pr_id, kind, actor, bot, sha, created_at)
    SELECT id, 'opened', author, 0, head_sha, created_at FROM pull_requests;

-- When knowledge was used: looked up by a tool (the CLI, MCP, an agent),
-- mentioned in text (a [[link]], a page code, a term or its alias), or a
-- glossary entry's word to avoid written instead of the term.
CREATE TABLE knowledge_uses (
    id         INTEGER PRIMARY KEY,
    item_type  TEXT    NOT NULL, -- glossary, page, design
    item_id    INTEGER NOT NULL,
    kind       TEXT    NOT NULL, -- lookup, mention, avoided
    src_type   TEXT    NOT NULL DEFAULT '', -- issue, comment, page, design, glossary, pr; '' for lookups
    src_id     INTEGER,
    word       TEXT    NOT NULL DEFAULT '', -- what matched: the term, an alias, the word to avoid, a code
    project_id INTEGER REFERENCES projects(id) ON DELETE CASCADE, -- where it happened
    user_id    INTEGER REFERENCES users(id) ON DELETE SET NULL,
    agent      TEXT    NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL
);
CREATE INDEX knowledge_uses_item ON knowledge_uses(item_type, item_id, created_at);
CREATE INDEX knowledge_uses_time ON knowledge_uses(created_at);
-- A mention counts once per source; editing the text again doesn't repeat it.
CREATE UNIQUE INDEX knowledge_uses_once ON knowledge_uses(item_type, item_id, kind, src_type, src_id, word) WHERE src_id IS NOT NULL;
CREATE TRIGGER knowledge_uses_glossary AFTER DELETE ON glossary BEGIN
    DELETE FROM knowledge_uses WHERE item_type = 'glossary' AND item_id = old.id;
END;
CREATE TRIGGER knowledge_uses_page AFTER DELETE ON pages BEGIN
    DELETE FROM knowledge_uses WHERE (item_type = 'page' AND item_id = old.id) OR (src_type = 'page' AND src_id = old.id);
END;
CREATE TRIGGER knowledge_uses_design AFTER DELETE ON design_notes BEGIN
    DELETE FROM knowledge_uses WHERE (item_type = 'design' AND item_id = old.id) OR (src_type = 'design' AND src_id = old.id);
END;
CREATE TRIGGER knowledge_uses_issue AFTER DELETE ON issues BEGIN
    DELETE FROM knowledge_uses WHERE src_type = 'issue' AND src_id = old.id;
END;
CREATE TRIGGER knowledge_uses_comment AFTER DELETE ON comments BEGIN
    DELETE FROM knowledge_uses WHERE src_type = 'comment' AND src_id = old.id;
END;
-- Links made so far are mentions from when they were made.
INSERT OR IGNORE INTO knowledge_uses(item_type, item_id, kind, src_type, src_id, word, project_id, user_id, agent, created_at)
    SELECT l.dst_type, l.dst_id, 'mention', l.src_type, l.src_id, '',
        CASE l.src_type
            WHEN 'issue' THEN (SELECT project_id FROM issues WHERE id = l.src_id)
            WHEN 'page' THEN (SELECT project_id FROM pages WHERE id = l.src_id)
            WHEN 'design' THEN (SELECT project_id FROM design_notes WHERE id = l.src_id)
            WHEN 'glossary' THEN (SELECT project_id FROM glossary WHERE id = l.src_id)
        END,
        l.created_by, l.created_agent, l.created_at
    FROM knowledge_links l WHERE l.dst_type IN ('glossary', 'page', 'design') AND l.dst_id IS NOT NULL;

-- What people decided: approvals of agents' work, delegation, pausing and
-- ordering agents, settings and access. Kept when a project is deleted.
CREATE TABLE audit_log (
    id          INTEGER PRIMARY KEY,
    user_id     INTEGER REFERENCES users(id) ON DELETE SET NULL,
    actor       TEXT    NOT NULL DEFAULT '', -- a forge login, when a forge reported it
    agent       TEXT    NOT NULL DEFAULT '', -- an agent acting through the person's token
    about_agent TEXT    NOT NULL DEFAULT '', -- the agent whose work or access it is about
    action      TEXT    NOT NULL,            -- review, design.approve, agent.pause, pr.merged, …
    category    TEXT    NOT NULL,            -- approval, delegation, control, settings, access
    project_id  INTEGER REFERENCES projects(id) ON DELETE SET NULL,
    target_type TEXT    NOT NULL DEFAULT '', -- issue, page, glossary, design, pr, automation, …
    target_id   INTEGER,
    target      TEXT    NOT NULL DEFAULT '', -- a label: WP-12, "Review pull requests", owner/repo#12
    url         TEXT    NOT NULL DEFAULT '',
    detail      TEXT    NOT NULL DEFAULT '', -- one line
    created_at  INTEGER NOT NULL
);
CREATE INDEX audit_log_time ON audit_log(created_at);
CREATE INDEX audit_log_project ON audit_log(project_id, created_at);
CREATE INDEX audit_log_user ON audit_log(user_id, created_at);

-- The record so far: people's reviews of agent-written text and of agents'
-- design changes, and delegations.
INSERT INTO audit_log(user_id, about_agent, action, category, project_id, target_type, target_id, target, detail, created_at)
    SELECT i.reviewed_by, i.text_agent, 'review', 'approval', i.project_id, 'issue', i.id, p.key || '-' || i.number,
        'reviewed text by ' || i.text_agent, i.reviewed_at
    FROM issues i JOIN projects p ON p.id = i.project_id
    WHERE i.reviewed_at IS NOT NULL AND i.reviewed_by IS NOT NULL;
INSERT INTO audit_log(user_id, about_agent, action, category, project_id, target_type, target_id, target, detail, created_at)
    SELECT pg.reviewed_by, pg.text_agent, 'review', 'approval', pg.project_id, 'page', pg.id, pg.title,
        'reviewed text by ' || pg.text_agent, pg.reviewed_at
    FROM pages pg WHERE pg.reviewed_at IS NOT NULL AND pg.reviewed_by IS NOT NULL;
INSERT INTO audit_log(user_id, about_agent, action, category, project_id, target_type, target_id, target, detail, created_at)
    SELECT g.reviewed_by, g.updated_agent, 'review', 'approval', g.project_id, 'glossary', g.id, g.term,
        'reviewed an entry by ' || g.updated_agent, g.reviewed_at
    FROM glossary g WHERE g.reviewed_at IS NOT NULL AND g.reviewed_by IS NOT NULL;
INSERT INTO audit_log(user_id, about_agent, action, category, project_id, target_type, target_id, target, detail, created_at)
    SELECT r.reviewed_by, r.agent, CASE r.state WHEN 'rejected' THEN 'design.reject' ELSE 'design.approve' END, 'approval',
        n.project_id, 'design', n.id, n.title,
        r.kind || ' by ' || r.agent || CASE WHEN r.review_note != '' THEN ': ' || r.review_note ELSE '' END,
        r.reviewed_at
    FROM design_revisions r JOIN design_notes n ON n.id = r.note_id
    WHERE r.agent != '' AND r.reviewed_at IS NOT NULL AND r.reviewed_by IS NOT NULL AND r.state IN ('approved', 'rejected', 'merged');
INSERT INTO audit_log(user_id, about_agent, action, category, project_id, target_type, target_id, target, detail, created_at)
    SELECT e.actor_id, CASE WHEN e.new_value = '' THEN e.old_value ELSE e.new_value END, CASE WHEN e.new_value = '' THEN 'undelegate' ELSE 'delegate' END, 'delegation',
        e.project_id, 'issue', e.issue_id, p.key || '-' || i.number,
        CASE WHEN e.new_value = '' THEN 'took it off agents (was ' || e.old_value || ')' ELSE 'for ' || e.new_value END,
        e.created_at
    FROM events e JOIN issues i ON i.id = e.issue_id JOIN projects p ON p.id = i.project_id
    WHERE e.kind = 'issue.updated' AND e.field = 'agent' AND e.actor_id IS NOT NULL
        AND e.via_automation IS NULL AND coalesce(json_extract(e.data, '$.via'), '') = '';
