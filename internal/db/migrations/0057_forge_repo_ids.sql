-- The forge's own id for an attached repo (Forgejo and GitHub repository id,
-- GitLab project id): it survives renames and transfers, so Waypoint follows
-- a repo to its new name instead of taking it for a new repo (WP-166). 0
-- until Waypoint first sees it.
ALTER TABLE project_repos ADD COLUMN forge_repo_id INTEGER NOT NULL DEFAULT 0;
CREATE INDEX project_repos_forge_repo_id ON project_repos(forge_id, forge_repo_id) WHERE forge_repo_id != 0;
