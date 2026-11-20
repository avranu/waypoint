-- Projects made before starter tags existed get them too (the same ones
-- new projects start with; see core.DefaultTags).
INSERT OR IGNORE INTO labels(project_id, name, color) SELECT id, 'ui', '#0ea5e9' FROM projects;
INSERT OR IGNORE INTO labels(project_id, name, color) SELECT id, 'backend', '#64748b' FROM projects;
