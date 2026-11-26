-- An audit finding that is about a point the change's ledger already held
-- (a review comment c12, or another audit's finding 12.3) names it
-- (WP-1912). "" is a finding about nothing earlier.
ALTER TABLE code_audit_findings ADD COLUMN refers_to TEXT NOT NULL DEFAULT '';
