-- A pull request link that moved its issue (an automation, or wp ship, put
-- the issue In Review) remembers the status ids it moved it from and to, so
-- removing the link can put the issue back (WP-162).
ALTER TABLE external_links ADD COLUMN moved_from INTEGER;
ALTER TABLE external_links ADD COLUMN moved_to INTEGER;
