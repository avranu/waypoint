-- A design note an agent wrote for a task a person filed is accepted on that
-- person's request instead of waiting as a proposal (WP-189): which task,
-- and the person who filed it. approved_by/approved_at hold the acceptance.
ALTER TABLE design_notes ADD COLUMN requested_issue_id INTEGER REFERENCES issues(id) ON DELETE SET NULL;
ALTER TABLE design_notes ADD COLUMN requested_by       INTEGER REFERENCES users(id)  ON DELETE SET NULL;

-- One-off backfill for exactly WP-ADR3, ADR4 and ADR5, each written to carry
-- out a task a person filed (WP-177, WP-176, WP-126), which that person
-- confirmed on 2026-10-26. Other proposals stay proposals. A no-op on a new
-- database.
UPDATE design_notes SET
    requested_issue_id = (SELECT i.id FROM issues i JOIN projects p ON p.id = i.project_id
        WHERE p.key = 'WP' AND i.number = CASE design_notes.number WHEN 3 THEN 177 WHEN 4 THEN 176 ELSE 126 END),
    requested_by = (SELECT i.reporter_id FROM issues i JOIN projects p ON p.id = i.project_id
        WHERE p.key = 'WP' AND i.number = CASE design_notes.number WHEN 3 THEN 177 WHEN 4 THEN 176 ELSE 126 END
        AND i.reporter_agent = ''),
    approved_by = (SELECT i.reporter_id FROM issues i JOIN projects p ON p.id = i.project_id
        WHERE p.key = 'WP' AND i.number = CASE design_notes.number WHEN 3 THEN 177 WHEN 4 THEN 176 ELSE 126 END
        AND i.reporter_agent = ''),
    approved_at = CAST(strftime('%s', 'now') AS INTEGER) * 1000
WHERE kind = 'decision' AND number IN (3, 4, 5) AND approved_at IS NULL AND created_agent <> ''
  AND project_id = (SELECT id FROM projects WHERE key = 'WP')
  AND (SELECT i.reporter_id FROM issues i JOIN projects p ON p.id = i.project_id
        WHERE p.key = 'WP' AND i.number = CASE design_notes.number WHEN 3 THEN 177 WHEN 4 THEN 176 ELSE 126 END
        AND i.reporter_agent = '') IS NOT NULL;
