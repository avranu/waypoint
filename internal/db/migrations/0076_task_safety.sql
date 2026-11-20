-- The safety screen (WP-270, WP-ADR10): before another agent takes a task
-- holding text an agent wrote and no person reviewed, rules and a model
-- read that text. One row per issue: the latest screen.
CREATE TABLE task_safety (
    issue_id   INTEGER PRIMARY KEY REFERENCES issues(id) ON DELETE CASCADE,
    text_hash  TEXT    NOT NULL,             -- the unreviewed agent-written text it read
    verdict    TEXT    NOT NULL,             -- pending, clear, hold
    flags      TEXT    NOT NULL DEFAULT '[]', -- the model's flags (JSON)
    rules      TEXT    NOT NULL DEFAULT '[]', -- rule hits (JSON)
    reason     TEXT    NOT NULL DEFAULT '',  -- what a person must check
    model      TEXT    NOT NULL DEFAULT '',
    error      TEXT    NOT NULL DEFAULT '',
    attempts   INTEGER NOT NULL DEFAULT 0,   -- failed model calls on this text
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);
CREATE INDEX task_safety_verdict ON task_safety(verdict);

-- The screen starts out with the prep models (free ones, with any paid
-- fallback an admin added), else the estimate models; clearing it under
-- Admin → AI leaves the rules alone.
INSERT INTO ai_tasks(task, provider_id, model, fallbacks)
SELECT 'task_safety', provider_id, model, fallbacks FROM ai_tasks WHERE task IN ('prep', 'estimate')
ORDER BY task = 'prep' DESC LIMIT 1
ON CONFLICT(task) DO NOTHING;
