-- What a dismissed goal guess was (WP-999 review). Dismissing a link
-- deleted its knowledge_links row first, so the no kept only which item
-- and goal: not how well the matcher scored the guess, nor who stood
-- behind it. These keep the link as it was when a person turned it down;
-- they stay empty (link_created_at NULL) for a no said without a link
-- (a suggestion dismissed). link_score is the matcher's similarity;
-- link_basis, link_by and link_agent are the link's basis, created_by and
-- created_agent as knowledge_links stored them (WP-ADR50).
ALTER TABLE goal_dismissals ADD COLUMN link_score REAL;
ALTER TABLE goal_dismissals ADD COLUMN link_basis TEXT NOT NULL DEFAULT '';
ALTER TABLE goal_dismissals ADD COLUMN link_by INTEGER REFERENCES users(id) ON DELETE SET NULL;
ALTER TABLE goal_dismissals ADD COLUMN link_agent TEXT NOT NULL DEFAULT '';
ALTER TABLE goal_dismissals ADD COLUMN link_created_at INTEGER;
