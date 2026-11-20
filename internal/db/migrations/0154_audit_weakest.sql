-- The weakest model an audit through the OpenLLMs gateway accepts (WP-916):
-- its quality bar (OpenLLMs ADR 0063, routing.weakest), by the loose name
-- the gateway rates, e.g. 'sonnet'. A review round of a change triage
-- found high-stakes asks for one; '' is no bar.
ALTER TABLE code_audits ADD COLUMN weakest TEXT NOT NULL DEFAULT '';
