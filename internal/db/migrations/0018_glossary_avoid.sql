-- Words a glossary entry steers away from: other names for the concept
-- that this project doesn't use ("invite to issue" for share), so a lookup
-- of one answers with the word to use instead (WP-D13).
ALTER TABLE glossary ADD COLUMN avoid TEXT NOT NULL DEFAULT '[]'; -- JSON array of strings
ALTER TABLE glossary_revisions ADD COLUMN avoid TEXT NOT NULL DEFAULT '[]';
