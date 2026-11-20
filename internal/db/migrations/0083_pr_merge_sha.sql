-- The commit a merged pull request became (WP-23, WP-D19): a squash or a
-- merge commit is what main's deployments run, not the pull request's head,
-- so "where it runs" matches it too. '' until the forge says.
ALTER TABLE pull_requests ADD COLUMN merge_sha TEXT NOT NULL DEFAULT '';
