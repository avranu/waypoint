-- Drift guard (WP-440, under WP-436). A pull request's footprint: what it
-- was when its first babysitting round was sent (the files it touched, its
-- size, its description), and the footprint checks made since, as JSON. A
-- fix-up round that spreads to other files, or grows the change by half
-- again, gets a strong model's check of the final change against the task.
ALTER TABLE pull_requests ADD COLUMN footprint TEXT NOT NULL DEFAULT '';

-- What an audit was asked for: '' an ordinary audit, footprint the drift
-- guard's check. A footprint check isn't the pull request's audit: it
-- doesn't stand in for one at a head, and its findings are scope creep.
ALTER TABLE code_audits ADD COLUMN purpose TEXT NOT NULL DEFAULT '';
