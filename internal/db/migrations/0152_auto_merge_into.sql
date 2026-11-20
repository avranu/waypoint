-- Merge automatically by base branch (WP-1041): which branches the setting
-- arms pull requests into. 'any' (every base, as before), 'default' (the
-- repo's default branch), 'others' (every branch but the default) or
-- 'match' (auto_merge_bases, a JSON list of branch globs).
ALTER TABLE project_repos ADD COLUMN auto_merge_into TEXT NOT NULL DEFAULT 'any';
ALTER TABLE project_repos ADD COLUMN auto_merge_bases TEXT NOT NULL DEFAULT '[]';
