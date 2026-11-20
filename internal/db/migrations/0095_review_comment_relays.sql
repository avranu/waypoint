-- WP-442: a review comment a mirror copied from GitHub keeps the GitHub
-- line comment it is of (relay_id, which a verdict reply names so it lands
-- in that thread) and who wrote it there (relay_author, whom the reply
-- addresses instead of the mirror's account).
ALTER TABLE review_comments ADD COLUMN relay_id     TEXT NOT NULL DEFAULT '';
ALTER TABLE review_comments ADD COLUMN relay_author TEXT NOT NULL DEFAULT '';
