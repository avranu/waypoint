-- A relation made because an issue's text named the other one (WP-781): the
-- issue whose text it was. NULL for a relation someone chose.
ALTER TABLE issue_relations ADD COLUMN named_in INTEGER REFERENCES issues(id) ON DELETE SET NULL;
CREATE INDEX issue_relations_named_in ON issue_relations(named_in) WHERE named_in IS NOT NULL;
