-- Usage trends (WP-196): moments that may change how agents spend tokens,
-- kept as they happen so before/after comparisons work on old data too.
-- kind 'instructions': a commit on a repo's default branch that changed an
-- agent instruction file (CLAUDE.md, AGENTS.md, anything under .claude/).
-- Recorded from push webhooks (at = when the push arrived) and, once per
-- repo, read back from the forge's history (at = the commit's time).
CREATE TABLE usage_marks (
    id       INTEGER PRIMARY KEY,
    forge_id INTEGER NOT NULL REFERENCES forges(id) ON DELETE CASCADE,
    repo     TEXT    NOT NULL,
    sha      TEXT    NOT NULL,
    kind     TEXT    NOT NULL DEFAULT 'instructions',
    at       INTEGER NOT NULL,
    author   TEXT    NOT NULL DEFAULT '',
    title    TEXT    NOT NULL DEFAULT '', -- the commit message's first line
    url      TEXT    NOT NULL DEFAULT '',
    paths    TEXT    NOT NULL DEFAULT '[]', -- the instruction files it changed
    UNIQUE (forge_id, repo, sha)
);
CREATE INDEX usage_marks_at ON usage_marks(at);
