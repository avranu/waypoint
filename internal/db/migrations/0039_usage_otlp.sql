-- OpenTelemetry as a fourth usage source (WP-D20, WP-26). Claude Code can
-- export api_request, tool_result and tool_decision events over OTLP/HTTP;
-- they land in the same tables as transcripts. `via` says which source a row
-- came from ('transcript' or 'otlp') so that a session read from its
-- transcript never also counts its OTLP events: the transcript is richer, and
-- its arrival deletes that session's OTLP rows (see usage_otlp.go).
ALTER TABLE usage_hours ADD COLUMN via TEXT NOT NULL DEFAULT 'transcript';
ALTER TABLE tool_hours ADD COLUMN via TEXT NOT NULL DEFAULT 'transcript';
ALTER TABLE permission_events ADD COLUMN via TEXT NOT NULL DEFAULT 'transcript';

DROP INDEX usage_hours_key;
CREATE UNIQUE INDEX usage_hours_key ON usage_hours(user_id, hour, machine, session, repo, model, kind, via);
DROP INDEX tool_hours_key;
CREATE UNIQUE INDEX tool_hours_key ON tool_hours(user_id, hour, machine, session, repo, model, kind, tool, detail, via);

-- OTLP events already applied (a hash of each event's time and attributes),
-- so an exporter's retry changes nothing. Old ones are pruned.
CREATE TABLE otlp_seen (
    user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    key        TEXT    NOT NULL,
    created_at INTEGER NOT NULL,
    PRIMARY KEY (user_id, key)
) WITHOUT ROWID;
CREATE INDEX otlp_seen_time ON otlp_seen(created_at);
