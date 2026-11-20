-- A task related to one done before it was filed is a follow-up of it
-- (WP-1003): found 'after' says Waypoint read the kind from that order,
-- where someone (a person, an agent, an automation) chose the link.
--
-- found loses its CHECK, as kind did in 0156: its values are in Go
-- (follow_ups.go), so a new one is a line there and not another rebuild.
CREATE TABLE issue_relations_new (
    id            INTEGER PRIMARY KEY,
    source_id     INTEGER NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
    target_id     INTEGER NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
    kind          TEXT    NOT NULL,
    created_at    INTEGER NOT NULL,
    named_in      INTEGER REFERENCES issues(id) ON DELETE SET NULL,
    found         TEXT,
    subkind       TEXT    NOT NULL DEFAULT '',
    basis         TEXT,
    created_by    INTEGER REFERENCES users(id) ON DELETE SET NULL,
    created_agent TEXT    NOT NULL DEFAULT '',
    score         REAL,
    UNIQUE (source_id, target_id, kind)
);
INSERT INTO issue_relations_new(id, source_id, target_id, kind, created_at, named_in, found, subkind, basis, created_by, created_agent, score)
    SELECT id, source_id, target_id, kind, created_at, named_in, found, subkind, basis, created_by, created_agent, score
    FROM issue_relations;
DROP TABLE issue_relations;
ALTER TABLE issue_relations_new RENAME TO issue_relations;
CREATE INDEX issue_relations_target ON issue_relations(target_id);
CREATE INDEX issue_relations_named_in ON issue_relations(named_in) WHERE named_in IS NOT NULL;

-- Plain relations made so far that are follow-ups by that rule: one end
-- done (and still done) before the other was filed, not parent and child,
-- no follow-up between them yet. Someone chose it: its basis says so, or,
-- made before 0156 recorded basis, an account's relation event on either
-- end within seconds of it (auto-relate acts as no account), so Waypoint's
-- own guesses stay plain relations.
WITH c AS (
    SELECT r.id,
        CASE WHEN tst.category = 'done' AND t.resolved_at < s.created_at THEN r.source_id ELSE r.target_id END AS later,
        CASE WHEN tst.category = 'done' AND t.resolved_at < s.created_at THEN r.target_id ELSE r.source_id END AS earlier
    FROM issue_relations r
    JOIN issues s ON s.id = r.source_id JOIN statuses sst ON sst.id = s.status_id
    JOIN issues t ON t.id = r.target_id JOIN statuses tst ON tst.id = t.status_id
    WHERE r.kind = 'relates' AND r.named_in IS NULL
      AND ((tst.category = 'done' AND t.resolved_at < s.created_at) OR (sst.category = 'done' AND s.resolved_at < t.created_at))
      AND coalesce(s.parent_id, 0) != t.id AND coalesce(t.parent_id, 0) != s.id
      AND NOT EXISTS (SELECT 1 FROM issue_relations f WHERE f.kind = 'follows'
          AND ((f.source_id = r.source_id AND f.target_id = r.target_id) OR (f.source_id = r.target_id AND f.target_id = r.source_id)))
      AND (r.basis IN ('', 'agent', 'automation')
          OR (r.basis IS NULL AND EXISTS (SELECT 1 FROM events e WHERE e.issue_id IN (r.source_id, r.target_id)
              AND e.field = 'relation' AND coalesce(e.actor_id, 0) > 0 AND abs(e.created_at - r.created_at) < 5000)))
)
UPDATE OR IGNORE issue_relations SET kind = 'follows', found = 'after', source_id = c.later, target_id = c.earlier
FROM c WHERE issue_relations.id = c.id;

-- A pair related twice (both ways) keeps the follow-up alone, which says
-- the same link; a plain row recorded from issue text (named_in) stays, as
-- its provenance is not on the follow-up.
DELETE FROM issue_relations WHERE kind = 'relates' AND named_in IS NULL AND EXISTS (SELECT 1 FROM issue_relations f
    WHERE f.kind = 'follows' AND f.found = 'after'
      AND ((f.source_id = issue_relations.source_id AND f.target_id = issue_relations.target_id)
        OR (f.source_id = issue_relations.target_id AND f.target_id = issue_relations.source_id)));
