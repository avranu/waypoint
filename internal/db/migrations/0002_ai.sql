-- AI providers (an opencode server, or any OpenAI-compatible API) and which
-- model handles each AI task.

CREATE TABLE ai_providers (
    id         INTEGER PRIMARY KEY,
    name       TEXT    NOT NULL,
    kind       TEXT    NOT NULL CHECK (kind IN ('opencode','openai')),
    base_url   TEXT    NOT NULL,
    api_key    TEXT    NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL
);

CREATE TABLE ai_tasks (
    task        TEXT    PRIMARY KEY,
    provider_id INTEGER NOT NULL REFERENCES ai_providers(id) ON DELETE CASCADE,
    model       TEXT    NOT NULL
);
