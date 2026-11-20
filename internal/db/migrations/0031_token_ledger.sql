-- Token ledger (WP-35): the per-call measures the hourly usage buckets can't
-- hold (an average needs each call's own context). Same key as usage_hours.
-- Sent by `wp usage sync` from the transcripts; older history is read again
-- once for these rows alone (the chunk id differs), so nothing double counts.
CREATE TABLE ledger_hours (
    id         INTEGER PRIMARY KEY,
    user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    hour       INTEGER NOT NULL, -- ms, start of the UTC hour
    machine    TEXT    NOT NULL,
    session    TEXT    NOT NULL,
    repo       TEXT    NOT NULL DEFAULT '',
    model      TEXT    NOT NULL,
    kind       TEXT    NOT NULL DEFAULT 'main',
    calls      INTEGER NOT NULL DEFAULT 0, -- calls measured
    ctx        INTEGER NOT NULL DEFAULT 0, -- their context summed: input + cache write + cache read
    over_200k  INTEGER NOT NULL DEFAULT 0, -- calls with over 200k of context
    over_300k  INTEGER NOT NULL DEFAULT 0,
    cr_300k    INTEGER NOT NULL DEFAULT 0, -- cache reads made by the calls over 300k
    cold       INTEGER NOT NULL DEFAULT 0, -- cache rebuilt after a gap longer than its lifetime
    cold_w5    INTEGER NOT NULL DEFAULT 0, -- tokens those calls wrote (5 minute cache)
    cold_w1    INTEGER NOT NULL DEFAULT 0, -- (1 hour cache)
    forks      INTEGER NOT NULL DEFAULT 0, -- subagents that began from their parent's context
    fork_ctx   INTEGER NOT NULL DEFAULT 0, -- their first call's context, summed
    fresh      INTEGER NOT NULL DEFAULT 0, -- subagents that began empty
    fresh_ctx  INTEGER NOT NULL DEFAULT 0
);
CREATE UNIQUE INDEX ledger_hours_key ON ledger_hours(user_id, hour, machine, session, repo, model, kind);
CREATE INDEX ledger_hours_session ON ledger_hours(user_id, session, hour);
