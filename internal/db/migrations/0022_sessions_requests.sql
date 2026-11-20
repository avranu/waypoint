-- Agent sessions and requests (WP-D16): which agents are working where, and
-- what they ask of a person while they keep working.

-- One run of a coding agent on one machine, filled by the CLI's hooks. Keyed
-- by (user, machine, the CLI's session id), so a resumed session is the same
-- row. `state` is what the last hook said; reading it applies the idle
-- (30 minutes) and ended (24 hours) rules.
CREATE TABLE agent_sessions (
    id           INTEGER PRIMARY KEY,
    user_id      INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    machine      TEXT    NOT NULL,
    cli_id       TEXT    NOT NULL,
    cli          TEXT    NOT NULL DEFAULT 'claude',
    agent        TEXT    NOT NULL DEFAULT '',
    model        TEXT    NOT NULL DEFAULT '',
    cwd          TEXT    NOT NULL DEFAULT '',
    repo         TEXT    NOT NULL DEFAULT '', -- owner/name
    branch       TEXT    NOT NULL DEFAULT '',
    worktree     INTEGER NOT NULL DEFAULT 0,
    project_id   INTEGER REFERENCES projects(id) ON DELETE SET NULL,
    issue_id     INTEGER REFERENCES issues(id) ON DELETE SET NULL, -- the task it holds
    title        TEXT    NOT NULL DEFAULT '',
    state        TEXT    NOT NULL DEFAULT 'working', -- working, waiting, idle, ended
    wait_reason  TEXT    NOT NULL DEFAULT '',
    last_prompt  TEXT    NOT NULL DEFAULT '',
    last_update  TEXT    NOT NULL DEFAULT '',
    input_tokens  INTEGER NOT NULL DEFAULT 0,
    output_tokens INTEGER NOT NULL DEFAULT 0,
    started_at   INTEGER NOT NULL,
    last_seen_at INTEGER NOT NULL,
    ended_at     INTEGER,
    UNIQUE (user_id, machine, cli_id)
);
CREATE INDEX agent_sessions_seen ON agent_sessions(last_seen_at);
CREATE INDEX agent_sessions_cli ON agent_sessions(user_id, cli_id);
CREATE INDEX agent_sessions_project ON agent_sessions(project_id, last_seen_at);
CREATE INDEX agent_sessions_cwd ON agent_sessions(user_id, machine, cwd);

-- What a session did and what happened to it: the timeline, and the counts
-- its summary is built from. Written in dispatch for events whose actor has
-- a session, and by the hooks.
CREATE TABLE session_events (
    id         INTEGER PRIMARY KEY,
    session_id INTEGER NOT NULL REFERENCES agent_sessions(id) ON DELETE CASCADE,
    kind       TEXT    NOT NULL, -- started, prompt, update, waiting, idle, ended, subagent, issue_created, issue_closed, comment, page, design, glossary, pr_linked, request, claimed, released
    ref_type   TEXT    NOT NULL DEFAULT '', -- issue, page, request, pr, …
    ref_id     INTEGER,
    label      TEXT    NOT NULL DEFAULT '', -- WP-12, R-4, a page title
    url        TEXT    NOT NULL DEFAULT '',
    detail     TEXT    NOT NULL DEFAULT '',
    project_id INTEGER REFERENCES projects(id) ON DELETE SET NULL,
    data       TEXT    NOT NULL DEFAULT '{}',
    created_at INTEGER NOT NULL
);
CREATE INDEX session_events_session ON session_events(session_id, id);

-- Which session a run on an issue came from.
ALTER TABLE agent_runs ADD COLUMN session_id INTEGER REFERENCES agent_sessions(id) ON DELETE SET NULL;
CREATE INDEX agent_runs_session ON agent_runs(session_id) WHERE session_id IS NOT NULL;

-- Something an agent asks of a person while it keeps working. Numbered per
-- project (R-12); outside a project it is personal, numbered for the person.
CREATE TABLE requests (
    id            INTEGER PRIMARY KEY,
    project_id    INTEGER REFERENCES projects(id) ON DELETE CASCADE,
    number        INTEGER NOT NULL,
    user_id       INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE, -- the person it is for
    session_id    INTEGER REFERENCES agent_sessions(id) ON DELETE SET NULL,
    issue_id      INTEGER REFERENCES issues(id) ON DELETE SET NULL,
    agent         TEXT    NOT NULL DEFAULT '',
    kind          TEXT    NOT NULL, -- question, change, command, permission, action
    title         TEXT    NOT NULL,
    body          TEXT    NOT NULL DEFAULT '',
    state         TEXT    NOT NULL DEFAULT 'open', -- open, answered, approved, denied, done, dismissed, withdrawn, applying, applied, failed, conflict
    payload       TEXT    NOT NULL DEFAULT '{}',   -- kind-specific: questions, files, tool call
    answer        TEXT    NOT NULL DEFAULT '',
    decision      TEXT    NOT NULL DEFAULT '',     -- the last verb: answer, approve, deny, done, dismiss
    note          TEXT    NOT NULL DEFAULT '',     -- what the person said with it
    via           TEXT    NOT NULL DEFAULT '',     -- terminal: answered in the agent's own terminal
    decided_by    INTEGER REFERENCES users(id) ON DELETE SET NULL,
    decided_at    INTEGER,
    delivered_at  INTEGER,                         -- the agent has been told
    snoozed_until INTEGER,
    apply_path    TEXT    NOT NULL DEFAULT '',     -- runner, pr
    result        TEXT    NOT NULL DEFAULT '{}',   -- {commit, branch, pr_url, reason}
    error         TEXT    NOT NULL DEFAULT '',
    tool_use_id   TEXT    NOT NULL DEFAULT '',
    dedupe_key    TEXT    NOT NULL DEFAULT '',
    created_at    INTEGER NOT NULL,
    updated_at    INTEGER NOT NULL
);
CREATE UNIQUE INDEX requests_number ON requests(coalesce(project_id, -user_id), number);
CREATE INDEX requests_user ON requests(user_id, state);
CREATE INDEX requests_project ON requests(project_id, state);
CREATE INDEX requests_session ON requests(session_id, state);
CREATE INDEX requests_tool ON requests(session_id, tool_use_id) WHERE tool_use_id != '';
CREATE INDEX requests_dedupe ON requests(session_id, dedupe_key, created_at) WHERE dedupe_key != '';
CREATE INDEX requests_undelivered ON requests(user_id) WHERE delivered_at IS NULL AND decided_at IS NOT NULL;

-- A command a person runs from a request, like a notebook cell.
CREATE TABLE request_cells (
    id         INTEGER PRIMARY KEY,
    request_id INTEGER NOT NULL REFERENCES requests(id) ON DELETE CASCADE,
    n          INTEGER NOT NULL,
    command    TEXT    NOT NULL,
    dir        TEXT    NOT NULL DEFAULT '',
    machine    TEXT    NOT NULL DEFAULT '',
    UNIQUE (request_id, n)
);

-- Each run of a cell, kept so Run again adds to the history.
CREATE TABLE request_cell_runs (
    id          INTEGER PRIMARY KEY,
    cell_id     INTEGER NOT NULL REFERENCES request_cells(id) ON DELETE CASCADE,
    command     TEXT    NOT NULL, -- as it was run (the person may have edited it)
    job_id      INTEGER,
    state       TEXT    NOT NULL DEFAULT 'queued', -- queued, running, done, failed, cancelled, expired
    exit_code   INTEGER,
    output      TEXT    NOT NULL DEFAULT '',
    truncated   INTEGER NOT NULL DEFAULT 0,
    error       TEXT    NOT NULL DEFAULT '',
    run_by      INTEGER REFERENCES users(id) ON DELETE SET NULL,
    created_at  INTEGER NOT NULL,
    finished_at INTEGER
);
CREATE INDEX request_cell_runs_cell ON request_cell_runs(cell_id, id);

-- A notification can link anywhere in the app, not only to an issue.
ALTER TABLE notifications ADD COLUMN url TEXT NOT NULL DEFAULT '';
