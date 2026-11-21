-- The transcript archive, second round (WP-1466, audit 1218): what each piece
-- redacted, what each machine says about its passes and its opt-out, what the
-- collector counts as lost, and the archive's size and quota refusals.
-- Unknown is NULL everywhere: a piece stored before this has no redaction
-- count, a machine that has not reported has no pass record.

-- Values the client's redaction blanked in the piece, by rule name (never the
-- values or the key names): redactions is the number of string values changed.
ALTER TABLE transcript_pieces ADD COLUMN redactions INTEGER;
ALTER TABLE transcript_pieces ADD COLUMN redaction_rules TEXT; -- JSON: rule -> count

-- What a machine reports about its passes (cmd/wp/usage_archive.go): how long
-- the last one took and the longest reported, why the last one stopped before
-- the end ('' ran to the end), how many were cut short since the machine began,
-- and whether the archive is switched off on it (WP_USAGE_ARCHIVE; 1 off).
ALTER TABLE usage_machines ADD COLUMN transcript_pass_ms INTEGER;
ALTER TABLE usage_machines ADD COLUMN transcript_pass_max_ms INTEGER;
ALTER TABLE usage_machines ADD COLUMN transcript_stop TEXT;
ALTER TABLE usage_machines ADD COLUMN transcript_cut INTEGER;
ALTER TABLE usage_machines ADD COLUMN transcript_off INTEGER;

-- What the collector check keeps: the total of what a collector counts as
-- lost (NULL: it counts none) with the total at the last telling, and how many
-- machines opted out.
ALTER TABLE collector_health ADD COLUMN dropped INTEGER;
ALTER TABLE collector_health ADD COLUMN dropped_told INTEGER;
ALTER TABLE collector_health ADD COLUMN dropped_alerted_at INTEGER;
ALTER TABLE collector_health ADD COLUMN off INTEGER;

-- The archive's size, kept as pieces come and go so a read does not scan
-- transcript_pieces (the triggers also see an account's pieces go with it),
-- and the uploads the quota (WAYPOINT_TRANSCRIPT_MAX_MB) refused.
CREATE TABLE transcript_totals (
    id            INTEGER PRIMARY KEY CHECK (id = 1),
    stored_bytes  INTEGER NOT NULL,
    pieces        INTEGER NOT NULL,
    refused       INTEGER NOT NULL DEFAULT 0,
    refused_at    INTEGER
);
INSERT INTO transcript_totals(id, stored_bytes, pieces) SELECT 1, COALESCE(SUM(stored_bytes), 0), COUNT(*) FROM transcript_pieces;
CREATE TRIGGER transcript_pieces_added AFTER INSERT ON transcript_pieces BEGIN
    UPDATE transcript_totals SET stored_bytes = stored_bytes + NEW.stored_bytes, pieces = pieces + 1 WHERE id = 1;
END;
CREATE TRIGGER transcript_pieces_removed AFTER DELETE ON transcript_pieces BEGIN
    UPDATE transcript_totals SET stored_bytes = stored_bytes - OLD.stored_bytes, pieces = pieces - 1 WHERE id = 1;
END;
