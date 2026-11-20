-- The LLM gateway gets a generic name (WP-1187): any OpenAI-compatible
-- endpoint, its extensions of that API declared in llm_gateway_extensions.
-- Its records were named for the one gateway this ran on first: the
-- provider's setting key, the once-only chain upgrades (all done, and gone
-- from the code), and the chain entry of audits it runs, openllms/<model>,
-- now gateway/<model>.

UPDATE server_settings SET key = 'llm_gateway_provider' WHERE key = 'openllms_provider';
DELETE FROM server_settings WHERE key IN ('openllms_models', 'openllms_review_routes', 'openllms_free_route', 'openllms_prep_purpose', 'openllms_only');

-- A gateway set up before extensions were declared had them all; startup
-- writes what the environment says from now on.
INSERT OR IGNORE INTO server_settings(key, value)
    SELECT 'llm_gateway_extensions', 'routes,fallbacks,routing,exclude,key,tasks,feedback,decide'
    FROM server_settings WHERE key = 'llm_gateway_provider';

UPDATE code_audits SET models = replace(models, '"cli":"openllms"', '"cli":"gateway"') WHERE models LIKE '%"cli":"openllms"%';
UPDATE code_audits SET model = 'gateway' || substr(model, 9) WHERE model LIKE 'openllms/%' OR model = 'openllms';
UPDATE code_audits SET attempts = replace(attempts, '"openllms/', '"gateway/') WHERE attempts LIKE '%"openllms/%';

UPDATE runner_jobs SET cli = 'gateway' WHERE cli = 'openllms';
UPDATE runner_jobs SET input = replace(input, '"cli":"openllms"', '"cli":"gateway"') WHERE input LIKE '%"cli":"openllms"%';

-- A rule's models are JSON strings: one that starts openllms/, or the CLI
-- "openllms"; the word elsewhere (a URL, a prompt) is someone's text.
UPDATE automations SET rule = replace(replace(rule, '"openllms/', '"gateway/'), '"openllms"', '"gateway"')
    WHERE rule LIKE '%"openllms/%' OR rule LIKE '%"openllms"%';
