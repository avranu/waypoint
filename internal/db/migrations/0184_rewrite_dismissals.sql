-- A removed inferred rewrite relation is remembered for the pair of tasks,
-- whichever of them ships next (WP-1370). 0182_rewrite_evidence kept it as a flag on the
-- evidence row, which is directional (the task whose change replaced lines,
-- the task whose lines they were): removing the relation from the other
-- task's side marked a row the other task's ship never reads, and that ship
-- made the relation again. One row per unordered pair, low_id < high_id.
CREATE TABLE rewrite_dismissals (
    low_id     INTEGER NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
    high_id    INTEGER NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
    created_at INTEGER NOT NULL,
    PRIMARY KEY (low_id, high_id),
    CHECK (low_id < high_id)
);
CREATE INDEX rewrite_dismissals_high ON rewrite_dismissals(high_id);

INSERT OR IGNORE INTO rewrite_dismissals(low_id, high_id, created_at)
    SELECT min(issue_id, other_id), max(issue_id, other_id), min(updated_at)
    FROM rewrite_evidence WHERE dismissed = 1 AND issue_id != other_id
    GROUP BY min(issue_id, other_id), max(issue_id, other_id);

-- The dismissed evidence rows go with the flag: they explained a relation
-- that is gone, and the pair is kept above.
DELETE FROM rewrite_evidence WHERE dismissed = 1;
ALTER TABLE rewrite_evidence DROP COLUMN dismissed;
