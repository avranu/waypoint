-- Organisation webhooks and the outbound webhook delivery log.

-- A webhook Waypoint added (or took over) on a forge organisation or
-- GitLab group, so every repo in it reaches Waypoint (what automatic
-- projects need). Repos it covers get no repo webhook of their own.
CREATE TABLE forge_org_hooks (
    id         INTEGER PRIMARY KEY,
    forge_id   INTEGER NOT NULL REFERENCES forges(id) ON DELETE CASCADE,
    org        TEXT    NOT NULL, -- organisation login or GitLab group path
    hook_id    TEXT    NOT NULL DEFAULT '',
    state      TEXT    NOT NULL DEFAULT 'pending', -- pending, installed, failed
    note       TEXT    NOT NULL DEFAULT '',
    checked_at INTEGER,
    seen_at    INTEGER, -- the last signed delivery from a repo in it
    created_at INTEGER NOT NULL
);
CREATE UNIQUE INDEX forge_org_hooks_org ON forge_org_hooks(forge_id, lower(org));

-- One row per outbound delivery (retries of the same delivery update it).
-- The body is kept so it can be sent again; headers aren't (the signature
-- is made afresh with the webhook's current secret). Pruned to the last 50
-- per webhook and 14 days.
CREATE TABLE webhook_deliveries (
    id          INTEGER PRIMARY KEY,
    webhook_id  INTEGER NOT NULL REFERENCES webhooks(id) ON DELETE CASCADE,
    delivery    TEXT    NOT NULL, -- X-Waypoint-Delivery
    event       TEXT    NOT NULL,
    attempts    INTEGER NOT NULL DEFAULT 1,
    status      INTEGER, -- HTTP status of the last attempt; NULL: no answer
    error       TEXT    NOT NULL DEFAULT '',
    duration_ms INTEGER NOT NULL DEFAULT 0,
    request     TEXT    NOT NULL DEFAULT '', -- '' when too big to keep
    response    TEXT    NOT NULL DEFAULT '', -- the start of the answer
    replay_of   INTEGER, -- the delivery this one sent again
    created_at  INTEGER NOT NULL
);
CREATE INDEX webhook_deliveries_hook ON webhook_deliveries(webhook_id, id);
