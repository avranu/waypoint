-- Review pipelines (WP-D14, WP-5): several reviewers and a verifier.
-- A finding several reviewers raised counts them; one the verifier checked
-- names the model that did.
ALTER TABLE bot_findings ADD COLUMN agreed INTEGER NOT NULL DEFAULT 1;
ALTER TABLE bot_findings ADD COLUMN verified_by TEXT NOT NULL DEFAULT '';

-- The verifier starts out as the review chain turned by one, so a different
-- model checks what the first one found. Turned off by clearing it in
-- Admin → AI; it is seeded only here, once.
INSERT INTO ai_tasks(task, provider_id, model, fallbacks)
SELECT 'verify_review',
       CASE WHEN json_array_length(fallbacks) > 0 THEN json_extract(fallbacks, '$[0].provider_id') ELSE provider_id END,
       CASE WHEN json_array_length(fallbacks) > 0 THEN json_extract(fallbacks, '$[0].model') ELSE model END,
       CASE WHEN json_array_length(fallbacks) > 0
            THEN json_insert(json_remove(fallbacks, '$[0]'), '$[#]', json_object('provider_id', provider_id, 'model', model))
            ELSE '[]' END
FROM ai_tasks WHERE task = 'review'
ON CONFLICT(task) DO NOTHING;
