-- Navigation traces (WP-250): one row per task a coding-agent session took,
-- measured from its transcript by `wp usage sync`: what the agent did between
-- taking the task and finishing it (calls by class, before and after the first
-- edit, the files it touched and how it came to them), so what Waypoint's
-- pointers save an agent can be measured. A row is sent again as its segment
-- grows and replaces the last. Rows stay as long as the task does.
CREATE TABLE task_traces (
    id             INTEGER PRIMARY KEY,
    user_id        INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    machine        TEXT    NOT NULL,
    session        TEXT    NOT NULL,
    seq            INTEGER NOT NULL,            -- the session's nth take, from 1
    issue_id       INTEGER NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
    started_at     INTEGER NOT NULL,            -- the wp next / wp take call (ms)
    last_at        INTEGER NOT NULL,            -- the last call counted
    ended          TEXT    NOT NULL DEFAULT '', -- ship, done, release, next (another task taken); '': still open
    model          TEXT    NOT NULL DEFAULT '', -- the model of most of its requests
    calls          INTEGER NOT NULL DEFAULT 0,  -- tool calls after the take, main thread
    locate         INTEGER NOT NULL DEFAULT 0,  -- finding files: Glob, Grep with no file, find/fd/ls/tree/git ls-files, wp code/brief
    infile         INTEGER NOT NULL DEFAULT 0,  -- searching inside named files: Grep on a file, grep/rg on a path
    reads          INTEGER NOT NULL DEFAULT 0,  -- Read, sed -n/cat/head/tail/…
    edits          INTEGER NOT NULL DEFAULT 0,
    delegated      INTEGER NOT NULL DEFAULT 0,  -- Agent/Task calls (a subagent's own calls are not counted)
    other          INTEGER NOT NULL DEFAULT 0,
    pre_calls      INTEGER NOT NULL DEFAULT 0,  -- the same, before the first edit (every call when there is none)
    pre_locate     INTEGER NOT NULL DEFAULT 0,
    pre_infile     INTEGER NOT NULL DEFAULT 0,
    pre_reads      INTEGER NOT NULL DEFAULT 0,
    pre_delegated  INTEGER NOT NULL DEFAULT 0,
    first_edit_at  INTEGER,                     -- NULL: no edit
    ctx_start      INTEGER NOT NULL DEFAULT 0,  -- context tokens (input + cache read + cache write) of the request that made the take call, so wp next's own output counts as growth
    ctx_first_edit INTEGER,                     -- context tokens of the request that made the first edit
    compacted      INTEGER NOT NULL DEFAULT 0,  -- 1: the context shrank between the two (a compaction), so their difference means nothing
    files          TEXT    NOT NULL DEFAULT '[]', -- JSON, first touch order, at most 80: [{"p": repo-relative path, "n": navigation calls (locate+infile+reads) in the segment before its first touch, "r": read or searched in, "e": edited, "s": an earlier tool result in the segment had named it, "c": created by the agent (first touch was a Write of a file not read before)}]
    updated_at     INTEGER NOT NULL,
    UNIQUE (user_id, machine, session, seq)
);
CREATE INDEX task_traces_issue ON task_traces(issue_id, started_at);
