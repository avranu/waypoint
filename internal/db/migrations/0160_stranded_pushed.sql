-- WP-843: committed work a session lets go of is pushed to the forge by the
-- machine it is on; the task's stranded_work row says where it went, or why
-- it couldn't go.
ALTER TABLE stranded_work ADD COLUMN pushed TEXT NOT NULL DEFAULT '';      -- the branch on the forge
ALTER TABLE stranded_work ADD COLUMN pushed_head TEXT NOT NULL DEFAULT ''; -- the commit pushed (or tried)
ALTER TABLE stranded_work ADD COLUMN push_error TEXT NOT NULL DEFAULT '';  -- why the push failed, "" when it didn't
