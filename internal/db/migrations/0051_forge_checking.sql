-- A forge says "doesn't merge" (mergeable = false) while it is still working
-- it out after a push or a base move (WP-138). Waypoint keeps when it last
-- read that answer, and since when it has said no, so a fresh "no" counts as
-- "still checking" until a read some time after the head or base changed
-- repeats it; and it re-reads the pull request soon rather than at the next
-- sync.
ALTER TABLE pull_requests ADD COLUMN mergeable_read_at  INTEGER; -- ms: the forge's answer was last read
ALTER TABLE pull_requests ADD COLUMN mergeable_since    INTEGER; -- ms: when the current "no" was first read (head or base changed before it)
ALTER TABLE pull_requests ADD COLUMN recheck_at         INTEGER; -- ms: read it from the forge again then
ALTER TABLE pull_requests ADD COLUMN recheck_n          INTEGER NOT NULL DEFAULT 0;
CREATE INDEX pull_requests_recheck ON pull_requests(recheck_at) WHERE recheck_at IS NOT NULL;
-- An armed merge the forge refused as "try again later": how many times, for
-- which head.
ALTER TABLE pull_requests ADD COLUMN merge_retry_n   INTEGER NOT NULL DEFAULT 0;
ALTER TABLE pull_requests ADD COLUMN merge_retry_sha TEXT    NOT NULL DEFAULT '';
