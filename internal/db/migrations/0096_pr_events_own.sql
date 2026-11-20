-- What Waypoint posted itself on a pull request (its replies and acks in a
-- round, its notes): the merge policy's quiet period counts what others
-- post, so a babysitting round's own replies don't start it over (WP-455).
-- Marked when the event is recorded, from its whole text and its author;
-- the stored body is cut short, and the marker comes at the end.
ALTER TABLE pr_events ADD COLUMN own INTEGER NOT NULL DEFAULT 0;
