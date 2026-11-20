-- Agents, workflows, pages, nested projects and CLI device logins (ADR 0007).

CREATE TABLE workflows (
    id         INTEGER PRIMARY KEY,
    project_id INTEGER REFERENCES projects(id) ON DELETE CASCADE, -- NULL: everyone's
    name       TEXT    NOT NULL,
    steps      TEXT    NOT NULL DEFAULT '[]', -- JSON [{name, agent, instructions}]
    created_at INTEGER NOT NULL
);

ALTER TABLE issues ADD COLUMN agent          TEXT    NOT NULL DEFAULT ''; -- agent spec; '' = humans only
ALTER TABLE issues ADD COLUMN reporter_agent TEXT    NOT NULL DEFAULT ''; -- set when an agent created it
ALTER TABLE issues ADD COLUMN category       TEXT    NOT NULL DEFAULT '';
ALTER TABLE issues ADD COLUMN workflow_id    INTEGER REFERENCES workflows(id) ON DELETE SET NULL;
ALTER TABLE issues ADD COLUMN step           TEXT    NOT NULL DEFAULT '';
ALTER TABLE issues ADD COLUMN claimed_by     TEXT    NOT NULL DEFAULT ''; -- agent identity holding the lease
ALTER TABLE issues ADD COLUMN claimed_until  INTEGER;
CREATE INDEX issues_agent ON issues(agent) WHERE agent != '';

ALTER TABLE api_tokens ADD COLUMN agent TEXT NOT NULL DEFAULT ''; -- agent identity, for agent tokens

CREATE TABLE agent_runs (
    id            INTEGER PRIMARY KEY,
    issue_id      INTEGER NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
    agent         TEXT    NOT NULL,
    step          TEXT    NOT NULL DEFAULT '',
    user_id       INTEGER REFERENCES users(id) ON DELETE SET NULL, -- whose token
    started_at    INTEGER NOT NULL,
    ended_at      INTEGER,
    input_tokens  INTEGER NOT NULL DEFAULT 0,
    output_tokens INTEGER NOT NULL DEFAULT 0,
    cost_usd      REAL    NOT NULL DEFAULT 0,
    outcome       TEXT    NOT NULL DEFAULT '' -- done, released, expired
);
CREATE INDEX agent_runs_issue ON agent_runs(issue_id);

ALTER TABLE projects ADD COLUMN parent_id INTEGER REFERENCES projects(id) ON DELETE SET NULL;

CREATE TABLE pages (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    parent_id  INTEGER REFERENCES pages(id) ON DELETE CASCADE,
    project_id INTEGER REFERENCES projects(id) ON DELETE CASCADE,
    owner_id   INTEGER REFERENCES users(id) ON DELETE SET NULL, -- author; sole reader of non-project pages
    title      TEXT    NOT NULL,
    body       TEXT    NOT NULL DEFAULT '',
    category   TEXT    NOT NULL DEFAULT '',
    position   REAL    NOT NULL DEFAULT 0,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    updated_by INTEGER REFERENCES users(id) ON DELETE SET NULL
);
CREATE INDEX pages_parent ON pages(parent_id);
CREATE INDEX pages_project ON pages(project_id);

CREATE TABLE page_issues (
    page_id  INTEGER NOT NULL REFERENCES pages(id) ON DELETE CASCADE,
    issue_id INTEGER NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
    PRIMARY KEY (page_id, issue_id)
);
CREATE INDEX page_issues_issue ON page_issues(issue_id);

CREATE VIRTUAL TABLE page_search USING fts5(title, body); -- rowid = pages.id

CREATE TABLE device_logins (
    device_hash TEXT    PRIMARY KEY, -- sha256 of the code the CLI polls with
    user_code   TEXT    NOT NULL UNIQUE, -- what the person types or sees
    name        TEXT    NOT NULL DEFAULT '',
    agent       TEXT    NOT NULL DEFAULT '',
    ip          TEXT    NOT NULL DEFAULT '', -- who asked, shown on the approval page
    user_agent  TEXT    NOT NULL DEFAULT '',
    user_id     INTEGER REFERENCES users(id) ON DELETE CASCADE, -- set on approval
    token       TEXT    NOT NULL DEFAULT '', -- handed to the CLI once, then cleared
    created_at  INTEGER NOT NULL,
    expires_at  INTEGER NOT NULL
);
