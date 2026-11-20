-- Our review's findings still open when a pull request merges (WP-367,
-- WP-ADR20): they become one audit to settle, and each finding keeps the
-- audit finding it became, so a verdict on the audit labels the finding.
ALTER TABLE bot_findings ADD COLUMN audit_finding_id INTEGER REFERENCES code_audit_findings(id) ON DELETE SET NULL;
CREATE INDEX bot_findings_audit ON bot_findings(audit_finding_id) WHERE audit_finding_id IS NOT NULL;

-- Review comments by others (not our runs) still unresolved when the pull
-- request merged: counted once, never acted on. NULL until counted.
ALTER TABLE pull_requests ADD COLUMN open_others_at_merge INTEGER;
