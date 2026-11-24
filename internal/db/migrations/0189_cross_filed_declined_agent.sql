-- declined_agent: the agent that wrote a cross-filed task's decline reason,
-- '' when a person declined. declined_by is a display name either way; only
-- this column says the reason is agent-written text (WP-D11).
ALTER TABLE cross_filed ADD COLUMN declined_agent TEXT NOT NULL DEFAULT '';
-- Declines from before this keep the mark they had: who declined isn't known to be a person.
UPDATE cross_filed SET declined_agent = CASE WHEN declined_by = '' THEN 'declining-agent' ELSE declined_by END WHERE declined_at IS NOT NULL AND declined_why != '';
