-- The files a pull request touches, for automations whose PR filter names
-- paths (WP-D14 phase 5). Read from the forge's diff only when a rule
-- needs them, and kept for the head they were read at (files_sha), so the
-- rules of every project the repo belongs to share one read per push.
ALTER TABLE pull_requests ADD COLUMN files TEXT NOT NULL DEFAULT '';
ALTER TABLE pull_requests ADD COLUMN files_sha TEXT NOT NULL DEFAULT '';
