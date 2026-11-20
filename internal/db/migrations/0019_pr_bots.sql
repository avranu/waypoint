-- Bots on pull requests (WP-D14): model fallbacks, pull requests, bot runs
-- and the findings they post.

-- An AI task's model is tried first, then these, in order:
-- [{"provider_id": 1, "model": "opencode/nemotron-3-ultra-free"}, …].
ALTER TABLE ai_tasks ADD COLUMN fallbacks TEXT NOT NULL DEFAULT '[]';

-- Bots act on a repo's pull requests unless this is off.
ALTER TABLE project_repos ADD COLUMN bots INTEGER NOT NULL DEFAULT 1;
-- Except a mirror (the same repo name on another forge, attached later):
-- its pull requests are twins of the first repo's.
UPDATE project_repos SET bots = 0 WHERE EXISTS (
    SELECT 1 FROM project_repos o WHERE o.project_id = project_repos.project_id
      AND lower(o.repo) = lower(project_repos.repo) AND o.forge_id != project_repos.forge_id AND o.id < project_repos.id);

-- A separate account for the bot's reviews and comments; the forge's token
-- posts them when empty.
ALTER TABLE forges ADD COLUMN bot_token TEXT NOT NULL DEFAULT '';

-- Pull requests (GitLab: merge requests) of attached repos, as last reported.
CREATE TABLE pull_requests (
    id          INTEGER PRIMARY KEY,
    forge_id    INTEGER NOT NULL REFERENCES forges(id) ON DELETE CASCADE,
    repo        TEXT    NOT NULL,
    number      INTEGER NOT NULL,
    title       TEXT    NOT NULL DEFAULT '',
    author      TEXT    NOT NULL DEFAULT '',
    url         TEXT    NOT NULL DEFAULT '',
    base_ref    TEXT    NOT NULL DEFAULT '',
    head_ref    TEXT    NOT NULL DEFAULT '',
    head_sha    TEXT    NOT NULL DEFAULT '',
    state       TEXT    NOT NULL DEFAULT 'open', -- open, draft, closed, merged
    labels      TEXT    NOT NULL DEFAULT '[]',
    bots_paused INTEGER NOT NULL DEFAULT 0,      -- "@waypoint pause"
    created_at  INTEGER NOT NULL,
    updated_at  INTEGER NOT NULL,
    UNIQUE (forge_id, repo, number)
);

-- A bot's job on a pull request: queued by an automation, run in the
-- background, kept as a record of what it did.
CREATE TABLE bot_runs (
    id            INTEGER PRIMARY KEY,
    project_id    INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    automation_id INTEGER REFERENCES automations(id) ON DELETE SET NULL,
    pr_id         INTEGER NOT NULL REFERENCES pull_requests(id) ON DELETE CASCADE,
    kind          TEXT    NOT NULL,              -- review
    head_sha      TEXT    NOT NULL,
    options       TEXT    NOT NULL DEFAULT '{}', -- the action's settings
    reason        TEXT    NOT NULL DEFAULT '',   -- what started it: pr_opened, command, …
    comment_id    TEXT    NOT NULL DEFAULT '',   -- the comment that asked, if one did
    status        TEXT    NOT NULL DEFAULT 'queued', -- queued, running, done, failed, cancelled, skipped
    attempts      INTEGER NOT NULL DEFAULT 0,
    not_before    INTEGER NOT NULL DEFAULT 0,
    summary       TEXT    NOT NULL DEFAULT '',
    error         TEXT    NOT NULL DEFAULT '',
    log           TEXT    NOT NULL DEFAULT '[]', -- each model call: model, outcome, ms
    review_url    TEXT    NOT NULL DEFAULT '',
    started_at    INTEGER,
    finished_at   INTEGER,
    created_at    INTEGER NOT NULL
);
CREATE INDEX bot_runs_queue ON bot_runs(status, not_before);
CREATE INDEX bot_runs_project ON bot_runs(project_id, created_at);
CREATE INDEX bot_runs_pr ON bot_runs(pr_id, created_at);

CREATE TABLE bot_findings (
    id         INTEGER PRIMARY KEY,
    run_id     INTEGER NOT NULL REFERENCES bot_runs(id) ON DELETE CASCADE,
    pr_id      INTEGER NOT NULL REFERENCES pull_requests(id) ON DELETE CASCADE,
    path       TEXT    NOT NULL DEFAULT '',
    line       INTEGER,
    severity   TEXT    NOT NULL,                 -- critical, high, medium, low
    claim      TEXT    NOT NULL,
    model      TEXT    NOT NULL DEFAULT '',
    placement  TEXT    NOT NULL DEFAULT 'inline', -- inline, body, withheld
    state      TEXT    NOT NULL DEFAULT 'open',   -- open, fixed, withdrawn
    created_at INTEGER NOT NULL
);
CREATE INDEX bot_findings_pr ON bot_findings(pr_id, state);

-- Every project reviews its pull requests, and reviews one when asked, as
-- new projects do (WP-D14; the rules match core.AutomationTemplates).
INSERT INTO automations(project_id, name, enabled, rule, position, created_at, updated_at)
SELECT id, 'Review pull requests', 1,
       '{"trigger":{"event":"pr_updated"},"pr":{"skip_labels":["dependencies","autorelease: pending"],"skip_heads":["release-please--*"]},"actions":[{"type":"review"}]}',
       100, CAST(strftime('%s','now') AS INTEGER) * 1000, CAST(strftime('%s','now') AS INTEGER) * 1000
FROM projects;
INSERT INTO automations(project_id, name, enabled, rule, position, created_at, updated_at)
SELECT id, 'Review when asked', 1,
       '{"trigger":{"event":"pr_command","command":"review"},"actions":[{"type":"review"}]}',
       101, CAST(strftime('%s','now') AS INTEGER) * 1000, CAST(strftime('%s','now') AS INTEGER) * 1000
FROM projects;
