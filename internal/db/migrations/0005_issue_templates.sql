-- Reusable starting points for new issues, per project.

CREATE TABLE issue_templates (
    id          INTEGER PRIMARY KEY,
    project_id  INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    name        TEXT    NOT NULL COLLATE NOCASE,
    type        TEXT    NOT NULL DEFAULT '',
    title       TEXT    NOT NULL DEFAULT '',
    description TEXT    NOT NULL DEFAULT '',
    labels      TEXT    NOT NULL DEFAULT '[]', -- JSON array of names
    points      REAL,
    subtasks    TEXT    NOT NULL DEFAULT '[]', -- JSON array of titles
    created_at  INTEGER NOT NULL,
    UNIQUE (project_id, name)
);
