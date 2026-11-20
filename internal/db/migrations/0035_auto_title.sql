-- AI titles (WP-67): a model writes a short title for a new issue whose title
-- is long text, and the text becomes the description.
--
-- The title model starts out as the estimate model and its fallbacks (the
-- same kind of cheap, short answer), so installs with AI set up get titles
-- without a visit to Admin → AI; clearing it there turns titles off. Seeded
-- only here, once.
INSERT INTO ai_tasks(task, provider_id, model, fallbacks)
SELECT 'title', provider_id, model, fallbacks FROM ai_tasks WHERE task = 'estimate'
ON CONFLICT(task) DO NOTHING;

-- Every existing project asks for it, like new ones do; each project can turn
-- it off in its settings.
UPDATE projects SET settings = json_set(settings, '$.auto_title', json('true'));
