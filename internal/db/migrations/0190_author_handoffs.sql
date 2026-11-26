-- WP-1878: a failed check on a just-shipped pull request goes to the session
-- that wrote it while that session waits in wp ship. author_handoffs is a JSON
-- list, one entry per head handed over (its checks, when, and how it ended:
-- fixed by a new head, fell back to a round, or cleared), kept in the database
-- so a restart neither loses the hold nor hands a head over twice.
ALTER TABLE babysits ADD COLUMN author_handoffs TEXT NOT NULL DEFAULT '';
