-- Batched wake-ups (WP-209): news that must wake an agent waits a while
-- when more of its babysits are about to say something, so one wake-up
-- carries them all. batch_until is when it goes regardless; sent_with is the
-- message whose runner job posted it along with others to the inbox socket.
ALTER TABLE session_messages ADD COLUMN batch_until INTEGER;
ALTER TABLE session_messages ADD COLUMN sent_with INTEGER;
