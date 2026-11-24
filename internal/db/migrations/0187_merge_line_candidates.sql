-- wplint:ignore migrations comment-only edit: names the new unreadable end_reason
-- The merge line pushes to no branch of a pull request that rides a
-- candidate; a front that can't ride one is brought up to date on its own
-- branch (WP-1659, WP-ADR152). One queue_stacks row is now one candidate
-- (its pr_id is the last head's pull request).
-- reran: when (ms) the candidate's failed jobs were run again, once; 0: not yet.
-- conflict_pr, conflict_ahead: for a candidate that ended as a conflict, the
-- pull request whose head didn't merge, and whether it merges onto the base
-- alone but not onto the heads ahead of it (1: it waits for them; 0: it
-- conflicts with the base, which a round resolves).
-- end_reason: one machine word for how it ended (merged, failed, conflict,
-- lost, no_run, no_merge, line_changed, base_moved, head_moved, refused,
-- duplicate, batch_split, base_red, unreadable), beside the note's human text; '' while live and for rows
-- from before.
ALTER TABLE queue_stacks ADD COLUMN reran INTEGER NOT NULL DEFAULT 0;
ALTER TABLE queue_stacks ADD COLUMN end_reason TEXT NOT NULL DEFAULT '';
ALTER TABLE queue_stacks ADD COLUMN conflict_pr INTEGER NOT NULL DEFAULT 0;
ALTER TABLE queue_stacks ADD COLUMN conflict_ahead INTEGER NOT NULL DEFAULT 0;
-- line_candidates: a repo's merge line builds candidates (on by default); off,
-- every pull request is tested on its own branch, as for a line that can't
-- build them.
ALTER TABLE project_repos ADD COLUMN line_candidates INTEGER NOT NULL DEFAULT 1;
