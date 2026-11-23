-- The merge line tests candidates and never pushes to a pull request's
-- branch (WP-1659, WP-ADR152). One queue_stacks row is now one candidate
-- (its pr_id is the last head's pull request).
-- reran: the candidate's failed jobs were run again once.
-- end_reason: one machine word for how it ended (merged, failed, conflict,
-- lost, line_changed, base_moved, head_moved, refused, duplicate,
-- batch_split), beside the note's human text; '' while live and for rows
-- from before.
ALTER TABLE queue_stacks ADD COLUMN reran INTEGER NOT NULL DEFAULT 0;
ALTER TABLE queue_stacks ADD COLUMN end_reason TEXT NOT NULL DEFAULT '';
-- line_candidates: a repo's merge line builds candidates (on by default); off,
-- every pull request is tested on its own branch, as for a line that can't
-- build them.
ALTER TABLE project_repos ADD COLUMN line_candidates INTEGER NOT NULL DEFAULT 1;
