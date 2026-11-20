-- Project look (WP-65): who decided a project's icon, colour and background.
--   auto       nobody yet; an AI model may pick one (new projects, and old ones
--              that were never styled)
--   generated  an AI model picked it; Regenerate may replace it
--   custom     a person chose it; only an explicit Regenerate replaces it
--   plain      a person reset it to the default; left alone
ALTER TABLE projects ADD COLUMN look TEXT NOT NULL DEFAULT 'auto'
    CHECK (look IN ('auto','generated','custom','plain'));

-- Projects someone already styled keep what they chose.
UPDATE projects SET look = 'custom'
 WHERE color != '#6366f1' OR icon != '' OR background != '';

-- The "look" AI task starts out with the estimate model (a small structured
-- answer, like an estimate); change it under Admin -> AI.
INSERT INTO ai_tasks(task, provider_id, model, fallbacks)
SELECT 'look', provider_id, model, fallbacks FROM ai_tasks WHERE task = 'estimate'
ON CONFLICT(task) DO NOTHING;
