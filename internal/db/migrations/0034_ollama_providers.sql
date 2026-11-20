-- waypoint:no-foreign-keys
-- Ollama becomes an AI provider kind (WP-D14, WP-6): its own API, not only
-- its OpenAI-compatible /v1. SQLite can't change a CHECK, so the table is
-- rebuilt; ai_tasks points at it, hence no foreign keys while it is.
CREATE TABLE ai_providers_new (
    id         INTEGER PRIMARY KEY,
    name       TEXT    NOT NULL,
    kind       TEXT    NOT NULL CHECK (kind IN ('opencode','openai','ollama')),
    base_url   TEXT    NOT NULL,
    api_key    TEXT    NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL
);
INSERT INTO ai_providers_new(id, name, kind, base_url, api_key, created_at)
SELECT id, name, kind, base_url, api_key, created_at FROM ai_providers;
DROP TABLE ai_providers;
ALTER TABLE ai_providers_new RENAME TO ai_providers;
