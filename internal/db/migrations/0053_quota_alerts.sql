-- When each person was last told their weekly quota jumped (WP-146), so a
-- sustained burn pushes once an hour at most.
CREATE TABLE quota_alerts (
    user_id INTEGER PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    at      INTEGER NOT NULL -- ms
);
