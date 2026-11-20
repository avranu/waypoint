-- Provenance and review of agent-written content, agents' own timers, and
-- page codes like PL13 (ADR 0011).

-- An issue's text (title and description): whose token wrote it last, and
-- the agent if one did ('' = a person). Agent-written text is unreviewed
-- until a person reviews it (or rewrites it).
ALTER TABLE issues ADD COLUMN text_by        INTEGER REFERENCES users(id) ON DELETE SET NULL;
ALTER TABLE issues ADD COLUMN text_agent     TEXT    NOT NULL DEFAULT '';
ALTER TABLE issues ADD COLUMN reviewed_by    INTEGER REFERENCES users(id) ON DELETE SET NULL;
ALTER TABLE issues ADD COLUMN reviewed_at    INTEGER;
-- Who closed it (set with resolved_at, cleared on reopening).
ALTER TABLE issues ADD COLUMN resolved_by    INTEGER REFERENCES users(id) ON DELETE SET NULL;
ALTER TABLE issues ADD COLUMN resolved_agent TEXT    NOT NULL DEFAULT '';

UPDATE issues SET text_by = reporter_id, text_agent = reporter_agent;
-- History names the agent behind a change in data.via (people's and
-- agents' changes have an actor; forge syncs and AI tasks don't, or go
-- through an automation).
WITH last AS (
    SELECT e.issue_id, e.actor_id,
        CASE WHEN e.actor_id IS NOT NULL AND e.via_automation IS NULL
            THEN coalesce(json_extract(e.data, '$.via'), '') ELSE '' END AS agent
    FROM events e
    WHERE e.id IN (SELECT max(x.id) FROM events x
        WHERE x.kind = 'issue.updated' AND x.field IN ('title', 'description') GROUP BY x.issue_id)
)
UPDATE issues SET text_by = last.actor_id, text_agent = last.agent FROM last WHERE last.issue_id = issues.id;
WITH last AS (
    SELECT e.issue_id, e.actor_id,
        CASE WHEN e.actor_id IS NOT NULL AND e.via_automation IS NULL
            THEN coalesce(json_extract(e.data, '$.via'), '') ELSE '' END AS agent
    FROM events e
    WHERE e.id IN (SELECT max(x.id) FROM events x WHERE x.kind = 'issue.status_changed' GROUP BY x.issue_id)
)
UPDATE issues SET resolved_by = last.actor_id, resolved_agent = last.agent
    FROM last WHERE last.issue_id = issues.id AND issues.resolved_at IS NOT NULL;

-- Comments an agent wrote (through a person's token: author_id is theirs).
ALTER TABLE comments ADD COLUMN agent       TEXT NOT NULL DEFAULT '';
ALTER TABLE comments ADD COLUMN reviewed_by INTEGER REFERENCES users(id) ON DELETE SET NULL;
ALTER TABLE comments ADD COLUMN reviewed_at INTEGER;
UPDATE comments SET agent = coalesce((SELECT json_extract(e.data, '$.via') FROM events e
    WHERE e.kind = 'issue.commented' AND e.issue_id = comments.issue_id
        AND json_extract(e.data, '$.comment_id') = comments.id
        AND e.actor_id IS NOT NULL AND e.via_automation IS NULL LIMIT 1), '');

-- Pages: who created one and who wrote its text last, as for issues.
ALTER TABLE pages ADD COLUMN created_agent TEXT    NOT NULL DEFAULT '';
ALTER TABLE pages ADD COLUMN text_by       INTEGER REFERENCES users(id) ON DELETE SET NULL;
ALTER TABLE pages ADD COLUMN text_agent    TEXT    NOT NULL DEFAULT '';
ALTER TABLE pages ADD COLUMN reviewed_by   INTEGER REFERENCES users(id) ON DELETE SET NULL;
ALTER TABLE pages ADD COLUMN reviewed_at   INTEGER;
UPDATE pages SET text_by = coalesce(updated_by, owner_id);

-- Glossary entries already record their agents; agents' wait for review too.
ALTER TABLE glossary ADD COLUMN reviewed_by INTEGER REFERENCES users(id) ON DELETE SET NULL;
ALTER TABLE glossary ADD COLUMN reviewed_at INTEGER;

-- Page codes: the category's prefix and a number, per project (or per
-- person, for personal pages: scope is -owner_id). Every code a page has
-- had stays here, so old codes keep resolving, and codes are never reused:
-- a deleted page's stay (page_id NULL), as do rows that only reserve
-- numbers (a project's "next free" code, for docs imported from elsewhere).
ALTER TABLE pages ADD COLUMN number INTEGER;
CREATE TABLE page_codes (
    scope   INTEGER NOT NULL, -- project id, or -owner id for personal pages
    prefix  TEXT    NOT NULL, -- PL, I, …
    number  INTEGER NOT NULL,
    page_id INTEGER REFERENCES pages(id) ON DELETE SET NULL,
    PRIMARY KEY (scope, prefix, number)
);
CREATE INDEX page_codes_page ON page_codes(page_id);

-- Time an agent tracks is its own: it doesn't stop its person's timer.
-- paused: stopped because the agent went idle; it restarts when it works.
ALTER TABLE time_entries ADD COLUMN agent  TEXT    NOT NULL DEFAULT '';
ALTER TABLE time_entries ADD COLUMN paused INTEGER NOT NULL DEFAULT 0;
DROP INDEX time_entries_one_running;
CREATE UNIQUE INDEX time_entries_one_running ON time_entries(user_id, agent) WHERE ended_at IS NULL;
