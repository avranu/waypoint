-- Every project moves a task along with its pull request (WP-496): In Review
-- when a linked pull request opens, Done when it merges. wp done leaves a
-- task In Review until its pull request merges and says the merge finishes
-- it; outside WP nothing did, so finished work piled up In Review. New
-- projects get both rules (core.AutomationTemplates, Default); this adds
-- them to the projects that exist. A project that already moves issues on
-- that trigger (a set_status action anywhere in its list) keeps its own rule, on or off (a session's follow-ups don't
-- count), and the In Review rule goes only where a status of that name
-- exists (the rule moves to it by name).
INSERT INTO automations(project_id, name, enabled, rule, position, created_at, updated_at)
SELECT p.id, 'Move to In Review when a PR opens', 1,
       '{"trigger":{"event":"pr_opened"},"if":"is:open","actions":[{"type":"set_status","status":"In Review"}]}',
       102, CAST(strftime('%s','now') AS INTEGER) * 1000, CAST(strftime('%s','now') AS INTEGER) * 1000
FROM projects p
WHERE EXISTS (SELECT 1 FROM statuses st WHERE st.project_id = p.id AND st.name = 'In Review')
  AND NOT EXISTS (SELECT 1 FROM automations a WHERE a.project_id = p.id AND a.session_id IS NULL
                  AND json_extract(a.rule, '$.trigger.event') = 'pr_opened'
                  AND EXISTS (SELECT 1 FROM json_each(a.rule, '$.actions') act WHERE json_extract(act.value, '$.type') = 'set_status'));

INSERT INTO automations(project_id, name, enabled, rule, position, created_at, updated_at)
SELECT p.id, 'Move to Done when a PR merges', 1,
       '{"trigger":{"event":"pr_merged"},"if":"is:open","actions":[{"type":"set_status","status":"done"}]}',
       103, CAST(strftime('%s','now') AS INTEGER) * 1000, CAST(strftime('%s','now') AS INTEGER) * 1000
FROM projects p
WHERE NOT EXISTS (SELECT 1 FROM automations a WHERE a.project_id = p.id AND a.session_id IS NULL
                  AND json_extract(a.rule, '$.trigger.event') = 'pr_merged'
                  AND EXISTS (SELECT 1 FROM json_each(a.rule, '$.actions') act WHERE json_extract(act.value, '$.type') = 'set_status'));
