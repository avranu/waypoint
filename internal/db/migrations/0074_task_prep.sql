-- Task prep (WP-212, WP-ADR8): before an agent takes a queued task, a runner
-- uploads a bundle of the code the task points at (from its checkout's
-- index), and a free model picks from it and reads the task critically.
-- One row per issue: the latest prep.
CREATE TABLE task_preps (
    issue_id   INTEGER PRIMARY KEY REFERENCES issues(id) ON DELETE CASCADE,
    text_hash  TEXT    NOT NULL,            -- the task's text (title, description, comments) it was made for
    repo       TEXT    NOT NULL DEFAULT '',
    commit_sha TEXT    NOT NULL DEFAULT '',
    machine    TEXT    NOT NULL DEFAULT '',
    state      TEXT    NOT NULL,            -- pending (the model is reading it), ready, failed
    bundle     TEXT    NOT NULL,            -- the runner's code bundle (JSON)
    notes      TEXT    NOT NULL DEFAULT '', -- the model's answer, only what checked out (JSON)
    model      TEXT    NOT NULL DEFAULT '',
    dropped    INTEGER NOT NULL DEFAULT 0,  -- files, symbols and tests it named that the bundle hadn't
    error      TEXT    NOT NULL DEFAULT '',
    held_out   INTEGER NOT NULL DEFAULT 0,  -- made but never shown to agents: the comparison group
    shown_at   INTEGER,                     -- when an agent first got it
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);

-- The prep model starts out as the estimate model and its fallbacks (free
-- models), like titles did; clearing it under Admin → AI turns prep off.
INSERT INTO ai_tasks(task, provider_id, model, fallbacks)
SELECT 'prep', provider_id, model, fallbacks FROM ai_tasks WHERE task = 'estimate'
ON CONFLICT(task) DO NOTHING;

-- On in every project, like new ones; each can turn it off in its settings.
UPDATE projects SET settings = json_set(settings, '$.auto_prep', json('true'));
