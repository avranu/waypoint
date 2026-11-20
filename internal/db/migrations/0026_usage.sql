-- Usage analytics (WP-D20): what a person's coding-agent sessions used,
-- read from Claude Code's transcripts, hooks and status line by `wp`, never
-- reported by the model. Everything here is one person's own data (an
-- admin may look too), keyed by user, machine and the CLI's session id so
-- the sessions list (WP-D16, agent_sessions) can join to it later.

-- Hourly buckets of assistant messages, per session, repo, model and agent
-- kind ('main' or a subagent type such as Explore).
CREATE TABLE usage_hours (
    id             INTEGER PRIMARY KEY,
    user_id        INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    hour           INTEGER NOT NULL, -- ms, start of the UTC hour
    machine        TEXT    NOT NULL,
    session        TEXT    NOT NULL,
    repo           TEXT    NOT NULL DEFAULT '', -- owner/name, else the directory's name
    model          TEXT    NOT NULL,
    kind           TEXT    NOT NULL DEFAULT 'main',
    messages       INTEGER NOT NULL DEFAULT 0,
    input          INTEGER NOT NULL DEFAULT 0, -- uncached input tokens
    output         INTEGER NOT NULL DEFAULT 0,
    cache_write_5m INTEGER NOT NULL DEFAULT 0,
    cache_write_1h INTEGER NOT NULL DEFAULT 0,
    cache_read     INTEGER NOT NULL DEFAULT 0
);
CREATE UNIQUE INDEX usage_hours_key ON usage_hours(user_id, hour, machine, session, repo, model, kind);
CREATE INDEX usage_hours_session ON usage_hours(user_id, session, hour);

-- Tool calls per hour. kind: builtin (Bash, Read, …), mcp (detail = server),
-- skill, command (a slash command), subagent (detail = its type), bash
-- (detail = a command head such as "git commit"; one row per head).
CREATE TABLE tool_hours (
    id           INTEGER PRIMARY KEY,
    user_id      INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    hour         INTEGER NOT NULL,
    machine      TEXT    NOT NULL,
    session      TEXT    NOT NULL,
    repo         TEXT    NOT NULL DEFAULT '',
    model        TEXT    NOT NULL DEFAULT '',
    kind         TEXT    NOT NULL,
    tool         TEXT    NOT NULL DEFAULT '',
    detail       TEXT    NOT NULL DEFAULT '',
    calls        INTEGER NOT NULL DEFAULT 0,
    errors       INTEGER NOT NULL DEFAULT 0,
    refusals     INTEGER NOT NULL DEFAULT 0, -- not run: classifier, hook, rule or person said no
    prompts      INTEGER NOT NULL DEFAULT 0, -- a person was asked and said yes
    result_bytes INTEGER NOT NULL DEFAULT 0
);
CREATE UNIQUE INDEX tool_hours_key ON tool_hours(user_id, hour, machine, session, repo, model, kind, tool, detail);
CREATE INDEX tool_hours_session ON tool_hours(user_id, session, hour);

-- Refused tool calls, one row each, with the reason and a short excerpt of
-- the input (at most 300 characters).
CREATE TABLE permission_events (
    id      INTEGER PRIMARY KEY,
    user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    at      INTEGER NOT NULL,
    machine TEXT    NOT NULL,
    session TEXT    NOT NULL,
    repo    TEXT    NOT NULL DEFAULT '',
    model   TEXT    NOT NULL DEFAULT '',
    tool    TEXT    NOT NULL,
    detail  TEXT    NOT NULL DEFAULT '', -- command head, MCP tool, skill…
    reason  TEXT    NOT NULL DEFAULT '', -- "Production Deploy", "hook: x.py", "safety check"…
    source  TEXT    NOT NULL DEFAULT '', -- classifier, hook, safety_check, rule, user
    excerpt TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX permission_events_time ON permission_events(user_id, at);
CREATE INDEX permission_events_session ON permission_events(user_id, session, at);

-- The subscription's quota windows, as the status line reported them.
CREATE TABLE quota_samples (
    id        INTEGER PRIMARY KEY,
    user_id   INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    at        INTEGER NOT NULL,
    span      TEXT    NOT NULL, -- the window: five_hour, seven_day
    percent   REAL    NOT NULL,
    resets_at INTEGER NOT NULL, -- ms
    machine   TEXT    NOT NULL DEFAULT '',
    session   TEXT    NOT NULL DEFAULT '',
    model     TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX quota_samples_time ON quota_samples(user_id, span, at);

-- What is configured on each machine, by name, so unused things can be listed.
-- kind: mcp, skill, command, agent, plugin, hook. scope: user, a project
-- directory's name, or a plugin.
CREATE TABLE usage_inventory (
    user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    machine TEXT    NOT NULL,
    kind    TEXT    NOT NULL,
    name    TEXT    NOT NULL,
    scope   TEXT    NOT NULL DEFAULT '',
    detail  TEXT    NOT NULL DEFAULT '',
    seen_at INTEGER NOT NULL,
    PRIMARY KEY (user_id, machine, kind, name, scope)
);

-- Each machine that reports: when it last synced and what the parser could
-- not read (Claude Code's transcript format is internal and changes).
CREATE TABLE usage_machines (
    user_id        INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    machine        TEXT    NOT NULL,
    last_sync_at   INTEGER,
    last_status_at INTEGER, -- the last status line report
    wp_version     TEXT    NOT NULL DEFAULT '',
    parsed         INTEGER NOT NULL DEFAULT 0, -- transcript lines understood
    skipped        INTEGER NOT NULL DEFAULT 0, -- lines of a kind not recognised, or unreadable
    unknown        TEXT    NOT NULL DEFAULT '{}', -- JSON: unrecognised line type → count
    PRIMARY KEY (user_id, machine)
);

-- Session metadata as transcripts name it (agent_sessions, WP-D16, is the
-- sessions list; this joins to it by user, machine and session).
CREATE TABLE usage_sessions (
    user_id     INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    machine     TEXT    NOT NULL,
    session     TEXT    NOT NULL,
    cwd         TEXT    NOT NULL DEFAULT '',
    repo        TEXT    NOT NULL DEFAULT '',
    branch      TEXT    NOT NULL DEFAULT '',
    version     TEXT    NOT NULL DEFAULT '', -- the CLI's
    title       TEXT    NOT NULL DEFAULT '',
    first_at    INTEGER,
    last_at     INTEGER,
    compactions INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (user_id, machine, session)
);
CREATE INDEX usage_sessions_last ON usage_sessions(user_id, last_at);

-- Upload batches already applied (one per file range), so a repeated upload
-- changes nothing. Old ones are pruned.
CREATE TABLE usage_chunks (
    user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    id         TEXT    NOT NULL,
    created_at INTEGER NOT NULL,
    PRIMARY KEY (user_id, id)
);
CREATE INDEX usage_chunks_time ON usage_chunks(created_at);

-- API-equivalent prices, USD per million tokens, by model-id prefix (the
-- longest match wins, so a dated id finds its family). 0 = unknown.
CREATE TABLE model_prices (
    model          TEXT PRIMARY KEY,
    input          REAL    NOT NULL DEFAULT 0,
    output         REAL    NOT NULL DEFAULT 0,
    cache_write_5m REAL    NOT NULL DEFAULT 0,
    cache_write_1h REAL    NOT NULL DEFAULT 0,
    cache_read     REAL    NOT NULL DEFAULT 0,
    updated_at     INTEGER NOT NULL,
    updated_by     INTEGER REFERENCES users(id) ON DELETE SET NULL
);
-- List prices as of 2026-10 (Claude API pricing: cache writes 1.25x input for
-- 5 minutes and 2x for 1 hour; reads 0.1x, except Opus 5.5 at 0.05x and
-- Fable/Mythos 5.1 at 0.025x). Editable in Admin → AI → Prices.
INSERT INTO model_prices(model, input, output, cache_write_5m, cache_write_1h, cache_read, updated_at) VALUES
    ('claude-fable-5-1',  10,   50,  12.5,   20,    0.25,  0),
    ('claude-mythos-5-1', 10,   50,  12.5,   20,    0.25,  0),
    ('claude-fable-5',    10,   50,  12.5,   20,    1,     0),
    ('claude-opus-5-5',    4,   20,   5,      8,    0.2,   0),
    ('claude-opus-5',      5,   25,   6.25,  10,    0.5,   0),
    ('claude-opus-4-8',    5,   25,   6.25,  10,    0.5,   0),
    ('claude-opus-4-7',    5,   25,   6.25,  10,    0.5,   0),
    ('claude-opus-4-6',    5,   25,   6.25,  10,    0.5,   0),
    ('claude-opus-4-5',    5,   25,   6.25,  10,    0.5,   0),
    ('claude-sonnet-5-5',  2,   10,   2.5,    4,    0.2,   0),
    ('claude-sonnet-5',    2,   10,   2.5,    4,    0.2,   0),
    ('claude-sonnet-4-6',  3,   15,   3.75,   6,    0.3,   0),
    ('claude-sonnet-4-5',  3,   15,   3.75,   6,    0.3,   0),
    ('claude-haiku-5-5',   0.1,  0.5, 0.125,  0.2,  0.01,  0),
    ('claude-haiku-4-5',   1,    5,   1.25,   2,    0.1,   0);
