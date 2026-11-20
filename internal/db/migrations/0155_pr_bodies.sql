-- Pull request descriptions written two ways (WP-55): the one wp ship
-- renders from the template (published), and the Summary a free model
-- rewrites from the same inputs, never the diff. While the mode is shadow
-- the model's version is only kept here, for comparing the two; with the
-- mode on model, a version that passes the checks replaces the published one.
CREATE TABLE pr_bodies (
    id            INTEGER PRIMARY KEY,
    pr_id         INTEGER NOT NULL REFERENCES pull_requests(id) ON DELETE CASCADE,
    issue_id      INTEGER REFERENCES issues(id) ON DELETE SET NULL,
    created_at    INTEGER NOT NULL,
    body          TEXT    NOT NULL,             -- the description as opened
    summary       TEXT    NOT NULL DEFAULT '',  -- its Summary, as rendered (the part the model rewrites)
    inputs        TEXT    NOT NULL DEFAULT '{}',-- what wp ship sent besides: -m, commits, test evidence (JSON)
    state         TEXT    NOT NULL DEFAULT 'pending', -- pending, written, failed, skipped
    tries         INTEGER NOT NULL DEFAULT 0,
    model         TEXT    NOT NULL DEFAULT '',
    model_summary TEXT    NOT NULL DEFAULT '',
    unsupported   TEXT    NOT NULL DEFAULT '[]',-- names, numbers, paths and links in model_summary the inputs don't hold
    ms            INTEGER NOT NULL DEFAULT 0,   -- how long the model took
    used          INTEGER NOT NULL DEFAULT 0,   -- 1: the model's version was published
    note          TEXT    NOT NULL DEFAULT '',  -- why not, or what failed
    written_at    INTEGER
);
CREATE INDEX pr_bodies_pending ON pr_bodies(state) WHERE state = 'pending';
CREATE INDEX pr_bodies_created ON pr_bodies(created_at);
