-- The transcript archive (WP-1466, ADR145): every session's full transcript,
-- main thread and sub-agents, kept raw so any metric can be derived again.
-- The bytes live compressed on the server's disk (WAYPOINT_TRANSCRIPT_DIR);
-- this table has one row per stored piece. A piece is a run of whole JSONL
-- lines: [from_off, to_off) are the byte offsets in the machine's file, so
-- pieces of one file and generation join end to start; raw_bytes is what was
-- stored (after redaction, so it need not equal to_off - from_off).
-- A file Claude Code rewrote (or truncated) starts the next gen; gens are
-- kept, never merged.
CREATE TABLE transcript_pieces (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id        INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    machine        TEXT    NOT NULL,
    session        TEXT    NOT NULL,
    file           TEXT    NOT NULL, -- 'main', or the path under <session>/ (subagents/agent-x.jsonl)
    kind           TEXT    NOT NULL, -- main, or the sub-agent's type
    gen            INTEGER NOT NULL,
    fp             TEXT    NOT NULL, -- what the client said identifies this generation of the file
    from_off       INTEGER NOT NULL,
    to_off         INTEGER NOT NULL,
    raw_bytes      INTEGER NOT NULL, -- uncompressed, as stored
    stored_bytes   INTEGER NOT NULL, -- compressed, on disk
    sha256         TEXT    NOT NULL, -- of the uncompressed bytes
    redacted       INTEGER NOT NULL, -- 1: the client ran its secret redaction before upload
    client_version TEXT    NOT NULL DEFAULT '',
    path           TEXT    NOT NULL, -- under the transcript directory
    created_at     INTEGER NOT NULL,
    UNIQUE (user_id, machine, session, file, gen, from_off)
);
CREATE INDEX transcript_pieces_session ON transcript_pieces(session, user_id);

-- What each machine says about its archive upload, as totals it replaces on
-- every report: bytes still waiting, and every upload that failed, was
-- refused or skipped, by reason. Nothing is dropped without being counted here.
ALTER TABLE usage_machines ADD COLUMN transcript_pending_bytes INTEGER NOT NULL DEFAULT 0;
ALTER TABLE usage_machines ADD COLUMN transcript_pending_files INTEGER NOT NULL DEFAULT 0;
ALTER TABLE usage_machines ADD COLUMN transcript_pending_since INTEGER; -- when pending last went above 0
ALTER TABLE usage_machines ADD COLUMN transcript_dropped TEXT NOT NULL DEFAULT '{}'; -- JSON: reason -> count
ALTER TABLE usage_machines ADD COLUMN transcript_status_at INTEGER;
ALTER TABLE usage_machines ADD COLUMN transcript_piece_at INTEGER; -- the last piece stored
