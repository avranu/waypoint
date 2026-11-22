-- Agent instructions (WP-1559, ADR146): text Waypoint adds to a session's
-- context at its start, at a sub-agent's start and after a compaction, to the
-- sessions it is for (main thread or sub-agent, model, agent type), in place
-- of lines in AGENTS.md that every agent reads whole.

-- One instruction. project_id NULL: every project. Its text and who it is for
-- live in its revisions; rev is the approved revision that is delivered, 0
-- while no person has approved one (an agent's proposal: delivered to nobody).
CREATE TABLE agent_instructions (
    id         INTEGER PRIMARY KEY,
    project_id INTEGER REFERENCES projects(id) ON DELETE CASCADE,
    rev        INTEGER NOT NULL DEFAULT 0,
    enabled    INTEGER NOT NULL DEFAULT 1,
    position   INTEGER NOT NULL DEFAULT 0, -- delivered in this order, then by id
    created_by INTEGER REFERENCES users(id) ON DELETE SET NULL,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);
CREATE INDEX agent_instructions_project ON agent_instructions(project_id);

-- Every change is a revision. A person's is approved as written; an agent's
-- is pending until a person approves or rejects it.
CREATE TABLE agent_instruction_revisions (
    id             INTEGER PRIMARY KEY,
    instruction_id INTEGER NOT NULL REFERENCES agent_instructions(id) ON DELETE CASCADE,
    rev            INTEGER NOT NULL,
    title          TEXT    NOT NULL,
    body           TEXT    NOT NULL,
    scope          TEXT    NOT NULL DEFAULT 'any',   -- main, subagent, any
    models         TEXT    NOT NULL DEFAULT '[]',    -- JSON: names matched inside the model id; empty: any model
    agent_types    TEXT    NOT NULL DEFAULT '[]',    -- JSON: globs on the sub-agent's type, ! leaves one out; empty: any
    moment         TEXT    NOT NULL DEFAULT 'start', -- start (and again after a compaction), compact (only after one)
    state          TEXT    NOT NULL,                 -- approved, pending, rejected
    user_id        INTEGER REFERENCES users(id) ON DELETE SET NULL,
    agent          TEXT    NOT NULL DEFAULT '',
    -- The agent session that wrote it and the task it held (ADR145); NULL: a person, or not known.
    session_id       INTEGER REFERENCES agent_sessions(id) ON DELETE SET NULL,
    session_issue_id INTEGER REFERENCES issues(id) ON DELETE SET NULL,
    created_at     INTEGER NOT NULL,
    reviewed_by    INTEGER REFERENCES users(id) ON DELETE SET NULL,
    reviewed_at    INTEGER,
    UNIQUE (instruction_id, rev)
);

-- What was answered to which hook (ADR145): one row per instruction that was
-- for the session, sent or not. outcome: sent (in the hook's answer; whether
-- it reached the context is read from the transcript, not assumed), cut (over
-- the size budget), unknown_model (it names models and the session's model
-- wasn't known). model is NULL when unknown. Nothing prunes these by age;
-- rows leave with their instruction.
CREATE TABLE agent_instruction_deliveries (
    id             INTEGER PRIMARY KEY,
    instruction_id INTEGER NOT NULL REFERENCES agent_instructions(id) ON DELETE CASCADE,
    rev            INTEGER NOT NULL,
    session_id     INTEGER REFERENCES agent_sessions(id) ON DELETE SET NULL,
    project_id     INTEGER REFERENCES projects(id) ON DELETE SET NULL,
    agent_id       TEXT    NOT NULL DEFAULT '', -- the sub-agent; '' the main thread
    agent_type     TEXT    NOT NULL DEFAULT '',
    model          TEXT,
    event          TEXT    NOT NULL,            -- SessionStart, SubagentStart
    source         TEXT    NOT NULL DEFAULT '', -- SessionStart's: startup, resume, clear, compact
    chars          INTEGER NOT NULL DEFAULT 0,
    outcome        TEXT    NOT NULL,
    created_at     INTEGER NOT NULL
);
CREATE INDEX agent_instruction_deliveries_instruction ON agent_instruction_deliveries(instruction_id, created_at);
CREATE INDEX agent_instruction_deliveries_session ON agent_instruction_deliveries(session_id) WHERE session_id IS NOT NULL;
