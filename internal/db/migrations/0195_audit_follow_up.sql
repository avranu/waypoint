-- A pull request's audit of a later head can be a follow-up to the audit of
-- an earlier head (WP-1938): over_ref is that earlier head ("" for a full
-- audit), followed_up the same commit once the runner made the range from it
-- ("" while the audit runs, or when the runner fell back to the whole change).
ALTER TABLE code_audits ADD COLUMN over_ref TEXT NOT NULL DEFAULT '';
ALTER TABLE code_audits ADD COLUMN followed_up TEXT NOT NULL DEFAULT '';
