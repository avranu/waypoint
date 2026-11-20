-- The paths a mirror keeps off the other forge, per forge (WP-459): when a
-- tool copies a forge's repos and their reviews to another forge, findings
-- on these globs (one a line) stay out of the review text it copies. They
-- were built in for every Forgejo forge before; a Forgejo forge connected
-- then keeps them, and one connected from now on has none until an admin
-- says something mirrors it. Fresh databases have no forges, so nothing.
ALTER TABLE forges ADD COLUMN mirror_private TEXT NOT NULL DEFAULT '';
UPDATE forges SET mirror_private = '**/*.sops.env' || char(10) || '.forgejo/**' WHERE kind = 'forgejo';
