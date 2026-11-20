-- An agent closing an issue (moving it to Done or Won't Do) is the agent's
-- judgment, so a person can review the close (WP-272). close_review is 1
-- when an agent closed it; close_reviewed_by/at say which person reviewed
-- the close and when. All three clear when the issue reopens. Closes from
-- before this migration aren't flagged: nobody asked for their review.
ALTER TABLE issues ADD COLUMN close_review      INTEGER NOT NULL DEFAULT 0;
ALTER TABLE issues ADD COLUMN close_reviewed_by INTEGER REFERENCES users(id) ON DELETE SET NULL;
ALTER TABLE issues ADD COLUMN close_reviewed_at INTEGER;
CREATE INDEX issues_close_unreviewed ON issues(project_id) WHERE close_review = 1 AND close_reviewed_at IS NULL;
