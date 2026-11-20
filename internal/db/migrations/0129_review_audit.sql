-- Grok first in autopilot's adversarial review rounds (WP-687): before a
-- round types its prompt, Waypoint has Grok 4.7 audit the task's pull
-- request through Cursor on the person's runner, and the round hands its
-- findings to the session to check and fix. When Grok can't (no runner with
-- Cursor signed in, the plan's pool spent, the audit failed or took too
-- long) the round falls back to its own prompt, which the session reviews
-- with its Sonnet reviewer.
--
-- audit_id: the audit the round waited for or used; audit_at when it was
--   linked (the wait runs from then).
-- audit_note: why Grok wasn't used ('' while it is, or may still be).
ALTER TABLE work_log ADD COLUMN audit_id INTEGER REFERENCES code_audits(id) ON DELETE SET NULL;
ALTER TABLE work_log ADD COLUMN audit_at INTEGER;
ALTER TABLE work_log ADD COLUMN audit_note TEXT NOT NULL DEFAULT '';
CREATE INDEX work_log_audit ON work_log(audit_id) WHERE audit_id IS NOT NULL;

-- Reviews before the pull request (WP-688): wp ship pushes the task's
-- branch, and when autopilot reviews tasks Waypoint holds the pull request
-- for the round, which then runs on the branch (Grok audits it there).
-- before_pr: JSON {project, repo, branch, base} of such a round; '' for a
--   round of a pull request already open.
ALTER TABLE work_log ADD COLUMN before_pr TEXT NOT NULL DEFAULT '';
