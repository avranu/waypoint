-- Repowise context for story point estimates (WP-80). A runner with a checkout
-- of the project's repo runs Repowise on the issue's text and reports the
-- files the work probably touches and their history; the estimator reads it
-- from here. One row per issue: query_hash says which text the context was
-- gathered for (an edited issue gathers again), checked_at when. An empty
-- context means "looked, nothing to say" (no Repowise, no index).
CREATE TABLE estimate_context (
    issue_id   INTEGER PRIMARY KEY REFERENCES issues(id) ON DELETE CASCADE,
    query_hash TEXT    NOT NULL,
    context    TEXT    NOT NULL DEFAULT '',
    checked_at INTEGER NOT NULL
);
