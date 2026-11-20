-- Review runs' token counts (0085) were never filled: Waypoint streams its
-- model calls without asking for the usage chunk, so every row is 0. Usage
-- is read from OpenLLMs, which logs every request (GET /v1/key/usage), not
-- kept a second time here (WP-603).
ALTER TABLE bot_runs DROP COLUMN review_tokens_in;
ALTER TABLE bot_runs DROP COLUMN review_tokens_out;
ALTER TABLE bot_runs DROP COLUMN verify_tokens_in;
ALTER TABLE bot_runs DROP COLUMN verify_tokens_out;
