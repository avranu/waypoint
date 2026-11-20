-- A pull request claimed by the session pushing to or merging it through
-- wp pr (WP-117): one holder at a time, expiring, gone when the pull request
-- merges or closes.
CREATE TABLE pr_claims (
  pull_id INTEGER PRIMARY KEY REFERENCES pull_requests(id) ON DELETE CASCADE,
  session_id INTEGER NOT NULL REFERENCES agent_sessions(id) ON DELETE CASCADE,
  claimed_at INTEGER NOT NULL,
  expires_at INTEGER NOT NULL
);

CREATE TRIGGER pr_claims_release AFTER UPDATE OF state ON pull_requests
WHEN NEW.state IN ('merged', 'closed')
BEGIN
  DELETE FROM pr_claims WHERE pull_id = NEW.id;
END;
