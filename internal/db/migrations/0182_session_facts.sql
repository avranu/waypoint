-- Per-event session facts (WP-1467, ADR145 point 2): one row per model call,
-- tool call, turn, compaction, injected file, sub-agent spawn and settings
-- change, derived on the server from the transcript archive (transcript_pieces)
-- by internal/transcript. Nothing here is collected from the machine: a new
-- metric is a new query, and a parser fix is a version bump that derives the
-- archive again.
--
-- Every row has the time of its own event (at, ms; NULL when the record had
-- none), the file and generation it was read from, and the parser version that
-- produced it. A column is NULL when the transcript did not carry the value
-- (never 0, never a default label). A column whose name ends in _inferred is
-- inferred from timing, not read from the record.
--
-- uid names the event itself: a message id, a tool-use id, a record uuid. It is
-- unique per session, so an event that appears in two generations of a file, or
-- in a sub-agent's file and the main thread's, is one row. A file's rows are
-- replaced as a whole when it is derived again.

-- Where each (file, generation) stands: the progress of its derivation.
CREATE TABLE session_fact_files (
    user_id        INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    machine        TEXT    NOT NULL,
    session        TEXT    NOT NULL,
    file           TEXT    NOT NULL,
    gen            INTEGER NOT NULL,
    parser_version INTEGER NOT NULL,           -- of the last attempt
    attempted_to   INTEGER NOT NULL,           -- the archive's end offset the last attempt read to
    derived_to     INTEGER,                    -- the same for the last success; NULL: never derived
    lines          INTEGER NOT NULL DEFAULT 0, -- non-empty lines of the last success
    bytes          INTEGER NOT NULL DEFAULT 0, -- what the parser read
    rows           INTEGER NOT NULL DEFAULT 0, -- fact rows it produced (before the session-wide dedupe)
    kept           INTEGER NOT NULL DEFAULT 0, -- fact rows stored: the rest were the same event in another file or generation
    unknown        TEXT    NOT NULL DEFAULT '{}', -- JSON: record type -> count the parser did not understand
    ignored        TEXT    NOT NULL DEFAULT '{}', -- JSON: record type -> count it understands and takes no fact from
    parse_ms       INTEGER,
    derived_at     INTEGER,                    -- last success (ms)
    error          TEXT,                       -- why the last attempt failed; NULL: it did not
    error_at       INTEGER,
    PRIMARY KEY (user_id, machine, session, file, gen)
) WITHOUT ROWID;

CREATE TABLE fact_model_calls (
    id              INTEGER PRIMARY KEY,
    user_id         INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    machine         TEXT    NOT NULL,
    session         TEXT    NOT NULL,
    file            TEXT    NOT NULL,
    gen             INTEGER NOT NULL,
    parser_version  INTEGER NOT NULL,
    uid             TEXT    NOT NULL,
    seq             INTEGER NOT NULL,
    at              INTEGER,           -- the call's first block was written
    thread          TEXT    NOT NULL,  -- main, or the sub-agent's id
    sidechain       INTEGER NOT NULL,  -- 1: not the main thread
    msg_id          TEXT,
    request_id      TEXT,
    model           TEXT,
    requested_model TEXT,
    effort          TEXT,
    per_turn_effort TEXT,
    query_source    TEXT,
    stop_reason     TEXT,
    input_tokens    INTEGER,
    output_tokens   INTEGER,
    thinking_tokens INTEGER,           -- part of output_tokens
    cache_read      INTEGER,
    cache_write     INTEGER,           -- all cache writes
    cache_write_5m  INTEGER,           -- NULL: the usage carried no split
    cache_write_1h  INTEGER,
    context_tokens  INTEGER,           -- input + cache_read + cache_write
    end_at          INTEGER,           -- the last block was written
    latency_ms      INTEGER,           -- request sent (api-request record) to end_at
    thinking_ms     INTEGER,
    prompt_id       TEXT,              -- the human prompt it belongs to
    turn_uid        TEXT,              -- fact_turns.uid, main thread only
    gap_ms          INTEGER,           -- since the thread's previous call ended
    cold_inferred   INTEGER            -- 1: wrote the cache after a gap longer than its life (inferred from timing)
);
CREATE UNIQUE INDEX fact_model_calls_uid ON fact_model_calls(user_id, machine, session, uid);

CREATE TABLE fact_tool_calls (
    id             INTEGER PRIMARY KEY,
    user_id        INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    machine        TEXT    NOT NULL,
    session        TEXT    NOT NULL,
    file           TEXT    NOT NULL,
    gen            INTEGER NOT NULL,
    parser_version INTEGER NOT NULL,
    uid            TEXT    NOT NULL,   -- the tool-use id
    seq            INTEGER NOT NULL,
    at             INTEGER,
    thread         TEXT    NOT NULL,
    msg_id         TEXT,               -- the model call that made it
    prompt_id      TEXT,
    tool           TEXT    NOT NULL,
    target         TEXT,               -- file path, URL (no query), command head, pattern, query, skill or recipient: clipped, never content
    target_kind    TEXT,               -- file, url, command, pattern, query, skill, recipient
    detail         TEXT,               -- recipient kind (SendMessage), sub-agent type (Agent)
    read_offset    INTEGER,
    read_limit     INTEGER,
    todo_items     INTEGER,            -- TodoWrite: items in the list
    plan_chars     INTEGER,            -- ExitPlanMode: size of the plan
    background     INTEGER,
    is_test        INTEGER,            -- an edit of a test file (NULL for tools that do not edit)
    result_at      INTEGER,            -- NULL for all below: no result was found
    duration_ms    INTEGER,            -- result time - call time (includes a wait for permission)
    result_bytes   INTEGER,
    is_error       INTEGER,
    error          TEXT,               -- first words of the error
    exit_code      INTEGER,            -- only when the error text says "Exit code N"
    interrupted    INTEGER,
    timed_out      INTEGER,
    lines_added    INTEGER,            -- edits, from the result's patch
    lines_removed  INTEGER
);
CREATE UNIQUE INDEX fact_tool_calls_uid ON fact_tool_calls(user_id, machine, session, uid);

CREATE TABLE fact_turns (
    id             INTEGER PRIMARY KEY,
    user_id        INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    machine        TEXT    NOT NULL,
    session        TEXT    NOT NULL,
    file           TEXT    NOT NULL,
    gen            INTEGER NOT NULL,
    parser_version INTEGER NOT NULL,
    uid            TEXT    NOT NULL,   -- the record that started it
    seq            INTEGER NOT NULL,
    at             INTEGER,
    thread         TEXT    NOT NULL,
    prompt_id      TEXT,
    origin         TEXT,               -- human, task_notification, peer, scheduled: as the transcript says; NULL: it does not
    prompt_source  TEXT,
    slash_command  TEXT,
    prompt_index   INTEGER,
    turn_index     INTEGER,
    prompt_bytes   INTEGER,            -- the size of the prompt, not the prompt
    permission_mode TEXT,
    duration_ms    INTEGER,            -- from the turn_duration record that ended it
    messages       INTEGER,
    ended_at       INTEGER
);
CREATE UNIQUE INDEX fact_turns_uid ON fact_turns(user_id, machine, session, uid);

CREATE TABLE fact_compactions (
    id             INTEGER PRIMARY KEY,
    user_id        INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    machine        TEXT    NOT NULL,
    session        TEXT    NOT NULL,
    file           TEXT    NOT NULL,
    gen            INTEGER NOT NULL,
    parser_version INTEGER NOT NULL,
    uid            TEXT    NOT NULL,
    seq            INTEGER NOT NULL,
    at             INTEGER,
    thread         TEXT    NOT NULL,
    trigger        TEXT,               -- auto, manual
    pre_tokens     INTEGER,
    post_tokens    INTEGER,
    dropped_tokens INTEGER,            -- cumulative over the session, as the record says
    duration_ms    INTEGER
);
CREATE UNIQUE INDEX fact_compactions_uid ON fact_compactions(user_id, machine, session, uid);

CREATE TABLE fact_injected_files (
    id             INTEGER PRIMARY KEY,
    user_id        INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    machine        TEXT    NOT NULL,
    session        TEXT    NOT NULL,
    file           TEXT    NOT NULL,
    gen            INTEGER NOT NULL,
    parser_version INTEGER NOT NULL,
    uid            TEXT    NOT NULL,   -- record uuid # position in the record
    seq            INTEGER NOT NULL,
    at             INTEGER,
    thread         TEXT    NOT NULL,
    kind           TEXT    NOT NULL,   -- instructions, file, skill_listing, skill, compact_file_reference, edited_file
    file_type      TEXT,               -- an instruction file's class: Project, AutoMem (memory), User, ...
    name           TEXT,               -- path, or skill name
    bytes          INTEGER,            -- size of the content; NULL: the record carries none
    lines          INTEGER,
    count          INTEGER             -- skill_listing: skills listed
);
CREATE UNIQUE INDEX fact_injected_files_uid ON fact_injected_files(user_id, machine, session, uid);

CREATE TABLE fact_spawns (
    id              INTEGER PRIMARY KEY,
    user_id         INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    machine         TEXT    NOT NULL,
    session         TEXT    NOT NULL,
    file            TEXT    NOT NULL,
    gen             INTEGER NOT NULL,
    parser_version  INTEGER NOT NULL,
    uid             TEXT    NOT NULL,  -- the tool-use id of the Agent call
    seq             INTEGER NOT NULL,
    at              INTEGER,
    thread          TEXT    NOT NULL,
    prompt_id       TEXT,
    agent_type      TEXT,              -- NULL: the call names none
    purpose         TEXT,              -- its description, clipped
    requested_model TEXT,
    background      INTEGER,
    agent_id        TEXT,              -- from the result: the sub-agent's id (its transcript is subagents/agent-ID.jsonl)
    resolved_model  TEXT,
    status          TEXT,
    result_at       INTEGER,
    duration_ms     INTEGER,
    tokens          INTEGER
);
CREATE UNIQUE INDEX fact_spawns_uid ON fact_spawns(user_id, machine, session, uid);

CREATE TABLE fact_settings (
    id             INTEGER PRIMARY KEY,
    user_id        INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    machine        TEXT    NOT NULL,
    session        TEXT    NOT NULL,
    file           TEXT    NOT NULL,
    gen            INTEGER NOT NULL,
    parser_version INTEGER NOT NULL,
    uid            TEXT    NOT NULL,
    seq            INTEGER NOT NULL,
    at             INTEGER,            -- NULL: the record has no time (see near_at)
    thread         TEXT    NOT NULL,
    name           TEXT    NOT NULL,   -- model, effort, per_turn_effort, permission_mode, git_branch, harness_version
    value          TEXT    NOT NULL,
    prev           TEXT,               -- the value before; NULL for the first seen
    near_at        INTEGER,            -- the latest time seen before a record that has none
    source         TEXT    NOT NULL    -- the record type it was read from
);
CREATE UNIQUE INDEX fact_settings_uid ON fact_settings(user_id, machine, session, uid);
