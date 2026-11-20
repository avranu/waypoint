-- Draft-first pull requests (WP-158): an agent's pull request opens with the
-- forge's "WIP: " title prefix so CI runs its fast tier; Waypoint drops the
-- prefix when nothing else is pending, and the full suite runs once before
-- the merge.
ALTER TABLE pull_requests ADD COLUMN wip_auto        INTEGER NOT NULL DEFAULT 0; -- Waypoint holds it as a draft and marks it ready
ALTER TABLE pull_requests ADD COLUMN ready_at        INTEGER; -- ms: Waypoint dropped "WIP: " (NULL: it is a draft, or a person did it)
ALTER TABLE pull_requests ADD COLUMN ready_sha       TEXT NOT NULL DEFAULT ''; -- the head it was marked ready at
-- The newest commit status id the head had when it was marked ready. The
-- draft tier's statuses are all at or below it, and the full run's are above,
-- whatever the clocks say.
ALTER TABLE pull_requests ADD COLUMN ready_status_id INTEGER NOT NULL DEFAULT 0;
-- The checks the head had then (JSON names): the full run must report each again.
ALTER TABLE pull_requests ADD COLUMN ready_checks TEXT NOT NULL DEFAULT '[]';
-- The forge's id of each check's newest status.
ALTER TABLE ci_checks ADD COLUMN status_id INTEGER NOT NULL DEFAULT 0;
