-- Questions Waypoint filed from the end of an agent's turn (WP-96).
-- source: '' (an agent or a hook filed it on purpose) or 'auto' (the Stop
-- hook found a question in the chat message). notify_at: when to tell the
-- person if the request is still open; NULL: nothing to send (yet or any more).
ALTER TABLE requests ADD COLUMN source TEXT NOT NULL DEFAULT '';
ALTER TABLE requests ADD COLUMN notify_at INTEGER;
CREATE INDEX requests_notify ON requests(notify_at) WHERE notify_at IS NOT NULL;
