-- Every request Waypoint sends an AI model (WP-375, WP-374): what asked,
-- which model answered and how, so `wp ai usage` answers the AI key's
-- questions without a copy of the database. Kept 30 days.
CREATE TABLE ai_calls (
    id          INTEGER PRIMARY KEY,
    at          INTEGER NOT NULL,             -- unix ms, when the request was sent
    task        TEXT    NOT NULL,             -- review, verify_review, task_safety, title, …; other when unknown
    provider_id INTEGER NOT NULL,             -- the AI provider asked (runner entries are runner jobs, not kept here)
    model       TEXT    NOT NULL,             -- the model that answered, else the one asked
    fallbacks   INTEGER NOT NULL DEFAULT 0,   -- how many more the request named (a gateway walks them)
    paid        INTEGER NOT NULL DEFAULT 0,
    outcome     TEXT    NOT NULL,             -- ok, rate_limit, quota, timeout, server, auth, unavailable, bad_output, error, cancelled
    status      INTEGER NOT NULL DEFAULT 0,   -- the HTTP status, 0 when none came back
    tokens_in   INTEGER NOT NULL DEFAULT 0,
    tokens_out  INTEGER NOT NULL DEFAULT 0,
    ms          INTEGER NOT NULL DEFAULT 0,
    sent        INTEGER NOT NULL DEFAULT 0    -- HTTP requests it took: the client retries a 429 with Retry-After, a refused schema
);
CREATE INDEX ai_calls_at ON ai_calls(at);
