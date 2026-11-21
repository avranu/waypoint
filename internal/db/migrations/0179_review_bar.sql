-- The minimum model a gateway request was held to, as the gateway reported
-- it (its X-OpenLLMs-Bar header: weakest, the model it chose, its quality,
-- the purpose and where the bar came from), kept with the request so a
-- weaker model's answer can be traced to the bar that applied (WP-1440).
-- '' when the request had none or the gateway did not say.
ALTER TABLE ai_calls ADD COLUMN bar TEXT NOT NULL DEFAULT '';
ALTER TABLE gateway_tasks ADD COLUMN bar TEXT NOT NULL DEFAULT '';
