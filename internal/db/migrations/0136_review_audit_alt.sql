-- An adversarial review round whose Grok audit couldn't run (WP-820): the
-- round's audit goes to a model other than the session's before the session
-- reviews on its own prompt.
--
-- audit_alt: why Grok wasn't available ('' while Grok is or may still be);
--   audit_id then is the other model's audit. audit_note stays the reason
--   the round fell back to its own prompt.
ALTER TABLE work_log ADD COLUMN audit_alt TEXT NOT NULL DEFAULT '';
