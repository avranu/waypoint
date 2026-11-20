-- WP-1320, after 0175: the commits a helper's run read, in columns of their
-- own, and a finding's lead column given to the panel's own findings.
--
-- audit_helper_runs.from_sha and to_sha are the range of the change a
-- panel check read ('' when it recorded none): a retry of the same round
-- takes a done run that read exactly the range it reads, as it is, instead
-- of running the check again, and the round's own review row names only the
-- runs of its current range.
ALTER TABLE audit_helper_runs ADD COLUMN from_sha TEXT NOT NULL DEFAULT '';
ALTER TABLE audit_helper_runs ADD COLUMN to_sha TEXT NOT NULL DEFAULT '';

-- code_audit_findings.lead was (0174) the one panel lead a reviewer said
-- its finding came from, while reviewers were handed the panel's leads.
-- They no longer are: lead is now the ids (check.n, comma-separated) of the
-- confirmed panel problems a finding is made of, so a finding with a lead
-- is the panel's. What the old rows said is kept in taken_lead, so that a
-- reviewer's finding of before is not read as the panel's.
ALTER TABLE code_audit_findings ADD COLUMN taken_lead TEXT NOT NULL DEFAULT '';
UPDATE code_audit_findings SET taken_lead = lead, lead = '' WHERE lead != '';
