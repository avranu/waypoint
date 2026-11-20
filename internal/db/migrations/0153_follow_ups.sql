-- Follow-ups (WP-255): "A follows B" says A is more work on what B did.
-- found says how Waypoint knew, for the efficiency figures: NULL someone
-- chose it (or the text named it), 'session' an agent filed A while working
-- on B, 'files' A's merged pull request changed mostly the files B's did.
-- SQLite can't change a CHECK in place, so rebuild the table.
CREATE TABLE issue_relations_new (
    id         INTEGER PRIMARY KEY,
    source_id  INTEGER NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
    target_id  INTEGER NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
    kind       TEXT    NOT NULL CHECK (kind IN ('blocks','relates','duplicates','follows')),
    created_at INTEGER NOT NULL,
    named_in   INTEGER REFERENCES issues(id) ON DELETE SET NULL,
    found      TEXT    CHECK (found IN ('session','files')),
    UNIQUE (source_id, target_id, kind)
);
INSERT INTO issue_relations_new(id, source_id, target_id, kind, created_at, named_in)
    SELECT id, source_id, target_id, kind, created_at, named_in FROM issue_relations;
DROP TABLE issue_relations;
ALTER TABLE issue_relations_new RENAME TO issue_relations;
CREATE INDEX issue_relations_target ON issue_relations(target_id);
CREATE INDEX issue_relations_named_in ON issue_relations(named_in) WHERE named_in IS NOT NULL;

-- Follow-ups an agent's session filed before there was a kind for them:
-- what an agent wrote while working on another task (WP-275), but not a
-- sub-task filed under that task, which is a slice of it (as CreateIssue).
INSERT OR IGNORE INTO issue_relations(source_id, target_id, kind, created_at, found)
    SELECT i.id, i.origin_issue_id, 'follows', i.created_at, 'session'
    FROM issues i JOIN issues o ON o.id = i.origin_issue_id
    WHERE i.origin_issue_id IS NOT NULL AND i.origin_issue_id != i.id
      AND (i.parent_id IS NULL OR i.parent_id != i.origin_issue_id);
