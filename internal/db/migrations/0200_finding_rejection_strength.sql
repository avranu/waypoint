-- WP-1825: an agent's rejection of an audit finding keeps what it said was
-- the evidence and the model and effort it ran as (decided_model,
-- decided_effort: what the hook or the job reported, '' when unknown), and how
-- that compared with the model that found it: at_least, or not_compared when
-- the finder isn't one Waypoint ranks (decided_strength, with the reason).
ALTER TABLE code_audit_findings ADD COLUMN evidence TEXT NOT NULL DEFAULT '';
ALTER TABLE code_audit_findings ADD COLUMN decided_model TEXT NOT NULL DEFAULT '';
ALTER TABLE code_audit_findings ADD COLUMN decided_effort TEXT NOT NULL DEFAULT '';
ALTER TABLE code_audit_findings ADD COLUMN decided_strength TEXT NOT NULL DEFAULT '';
