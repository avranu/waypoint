-- The merge queue (WP-222): when an armed pull request joined its base's
-- queue. Only the front of a queue is brought up to date with its base and
-- gets its full run, so each merge costs one full run, not one per open
-- pull request. NULL: not in a queue.
ALTER TABLE pull_requests ADD COLUMN queued_at INTEGER;
CREATE INDEX pull_requests_queue ON pull_requests(forge_id, repo, base_ref, queued_at) WHERE queued_at IS NOT NULL;
