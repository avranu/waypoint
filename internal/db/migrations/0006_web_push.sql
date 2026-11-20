-- Web push: the server's VAPID keys and each browser's subscription.

CREATE TABLE server_settings (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

CREATE TABLE push_subscriptions (
    id           INTEGER PRIMARY KEY,
    user_id      INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    endpoint     TEXT    NOT NULL UNIQUE,
    p256dh       TEXT    NOT NULL,
    auth         TEXT    NOT NULL,
    user_agent   TEXT    NOT NULL DEFAULT '',
    created_at   INTEGER NOT NULL
);
CREATE INDEX push_subscriptions_user ON push_subscriptions(user_id);
