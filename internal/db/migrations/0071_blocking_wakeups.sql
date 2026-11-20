-- Blocking wake-ups (WP-209): news an agent is blocked on (wp ship
-- --blocking, wp request ask --blocking, a tell_agent step) goes within
-- minutes; everything else that wakes it waits in its session's batch.
-- Answers to requests batch too, and one runner job (message_batches) posts
-- a batch's messages and answers to the agent's inbox socket together.
ALTER TABLE babysits ADD COLUMN blocking INTEGER NOT NULL DEFAULT 0;
ALTER TABLE session_messages ADD COLUMN blocking INTEGER NOT NULL DEFAULT 0;
ALTER TABLE requests ADD COLUMN blocking INTEGER NOT NULL DEFAULT 0;
ALTER TABLE requests ADD COLUMN batch_until INTEGER;
ALTER TABLE requests ADD COLUMN sent_with INTEGER;
CREATE TABLE message_batches (
    id         INTEGER PRIMARY KEY,
    session_id INTEGER NOT NULL REFERENCES agent_sessions(id) ON DELETE CASCADE,
    created_at INTEGER NOT NULL
);
