-- Who put a design note under its parent: parent_by is the person who did it, NULL for an agent or anyone unknown; parent_at is set whenever a parent is set, and NULL (rows from before this) keeps the old rule, approved_at or requested_by counts as a person's.
ALTER TABLE design_notes ADD COLUMN parent_by INTEGER REFERENCES users(id) ON DELETE SET NULL;
ALTER TABLE design_notes ADD COLUMN parent_at INTEGER;
