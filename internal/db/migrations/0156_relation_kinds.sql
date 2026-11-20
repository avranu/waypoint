-- Relation kinds and who stands behind a relation (WP-1036, WP-ADR105).
--
-- kind loses its CHECK: the kinds are a registry in Go (relation_kinds.go),
-- so a new one is a line there and not another rebuild. New kinds here:
-- 'tension' (two tasks pull against each other; symmetric) and
-- 'supersedes' ("A supersedes B": A replaces B's plan or work).
--
-- subkind narrows a kind ('' none): a follow-up is a 'fix' (of something
-- the earlier work got wrong or missed) or an 'expansion' (more of what it
-- did). The registry says which kinds take which.
--
-- Who stands behind it, as knowledge_links (WP-ADR50): basis '' a person
-- made it directly, 'agent', 'automation', 'inferred' Waypoint found it
-- (found or named_in say how); NULL made before this migration and not
-- recorded. created_by/created_agent: the account and agent that made it.
-- score: how sure Waypoint was, where it measured that (for found =
-- 'files', the share of the task's files the other's pull request changed).
CREATE TABLE issue_relations_new (
    id            INTEGER PRIMARY KEY,
    source_id     INTEGER NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
    target_id     INTEGER NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
    kind          TEXT    NOT NULL,
    created_at    INTEGER NOT NULL,
    named_in      INTEGER REFERENCES issues(id) ON DELETE SET NULL,
    found         TEXT    CHECK (found IN ('session','files')),
    subkind       TEXT    NOT NULL DEFAULT '',
    basis         TEXT,
    created_by    INTEGER REFERENCES users(id) ON DELETE SET NULL,
    created_agent TEXT    NOT NULL DEFAULT '',
    score         REAL,
    UNIQUE (source_id, target_id, kind)
);
INSERT INTO issue_relations_new(id, source_id, target_id, kind, created_at, named_in, found, basis)
    SELECT id, source_id, target_id, kind, created_at, named_in, found,
        CASE WHEN found IS NOT NULL OR named_in IS NOT NULL THEN 'inferred' END
    FROM issue_relations;
DROP TABLE issue_relations;
ALTER TABLE issue_relations_new RENAME TO issue_relations;
CREATE INDEX issue_relations_target ON issue_relations(target_id);
CREATE INDEX issue_relations_named_in ON issue_relations(named_in) WHERE named_in IS NOT NULL;
