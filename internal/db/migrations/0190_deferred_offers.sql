-- WP-1858: a row means this session was offered this task as deferred work
-- when it shipped; it is the only de-duplication, so a task is offered to a
-- session once. via: ship now; message and idle later.
CREATE TABLE deferred_offers (
    id             INTEGER PRIMARY KEY,
    session_id     INTEGER NOT NULL REFERENCES agent_sessions(id) ON DELETE CASCADE,
    issue_id       INTEGER NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
    task_issue_id  INTEGER REFERENCES issues(id) ON DELETE SET NULL, -- the task being shipped
    via            TEXT    NOT NULL,
    context_tokens INTEGER,
    compacted      INTEGER NOT NULL DEFAULT 0,
    offered_at     INTEGER NOT NULL,
    told_at        INTEGER, -- when the text was queued for the session; NULL while it waits (a compaction first, or a send that failed)
    shipped_at     INTEGER, -- a wp ship that found this row untold and shipped: its text then says the session shipped
    UNIQUE (session_id, issue_id)
);
CREATE INDEX deferred_offers_issue ON deferred_offers(issue_id);
CREATE INDEX deferred_offers_untold ON deferred_offers(session_id) WHERE told_at IS NULL;
CREATE INDEX issues_session ON issues(session_id) WHERE session_id IS NOT NULL;
