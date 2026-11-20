-- The account a forge's mirror posts its copies of the other forge's reviews
-- as (WP-459). Only that account's relays count as the copied reviewer's:
-- a relay's marker names whatever account its poster typed, so it proves
-- nothing. A forge gets the account that posted the relays already stored
-- on it, the most if more than one, so a site whose mirror's copies counted
-- before still has them counted; a forge with none has no mirror account.
ALTER TABLE forges ADD COLUMN mirror_account TEXT NOT NULL DEFAULT '';
UPDATE forges SET mirror_account = coalesce((
    SELECT c.author FROM review_comments c JOIN pull_requests p ON p.id = c.pr_id
    WHERE p.forge_id = forges.id AND c.relay_author != ''
    GROUP BY c.author ORDER BY count(*) DESC, c.author LIMIT 1), '');
