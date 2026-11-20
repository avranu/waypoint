-- Pull requests whose work already landed, and merged branches on the forge
-- (WP-140).
--
-- delete_merged_branches: Waypoint deletes a branch on the forge once its
-- tip is in the default branch and nothing needs it (a daily sweep, and
-- right after a landed pull request is closed). On for every repo.
ALTER TABLE project_repos ADD COLUMN delete_merged_branches INTEGER NOT NULL DEFAULT 1;
-- When the repo's branches were last swept (the sweep runs daily).
ALTER TABLE project_repos ADD COLUMN branches_swept_at INTEGER;
-- A pull request whose head is already in its base: when Waypoint first saw
-- that (GitHub and GitLab are given minutes to mark it merged themselves),
-- and when it commented that it landed and finished it (never twice).
ALTER TABLE pull_requests ADD COLUMN landed_seen_at INTEGER;
ALTER TABLE pull_requests ADD COLUMN landed_at INTEGER;
