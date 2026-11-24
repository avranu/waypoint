-- ready_cand: the merge line's candidate (queue_stacks.id) a pull request was
-- marked ready on at ready_sha; 0 when its readiness came from anything else.
-- A candidate that ends takes readiness back from exactly these, not from
-- whichever of its heads happens to be ready (WP-1700, c6421/c6438).
ALTER TABLE pull_requests ADD COLUMN ready_cand INTEGER NOT NULL DEFAULT 0;
