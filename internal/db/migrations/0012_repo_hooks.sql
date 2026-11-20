-- Waypoint adds the forge webhook itself when a repo is attached, and
-- remembers how that went. hook_state is '' for repos attached before this
-- (their webhook, if any, was set up by hand), then pending, installed or
-- failed. hook_note says why it failed, or warns about an installed one.
ALTER TABLE project_repos ADD COLUMN hook_id TEXT NOT NULL DEFAULT '';
ALTER TABLE project_repos ADD COLUMN hook_state TEXT NOT NULL DEFAULT '';
ALTER TABLE project_repos ADD COLUMN hook_note TEXT NOT NULL DEFAULT '';
ALTER TABLE project_repos ADD COLUMN hook_checked_at INTEGER;
-- The last signed delivery from the forge about this repo.
ALTER TABLE project_repos ADD COLUMN hook_seen_at INTEGER;
