-- A task's work log (WP-285, WP-ADR38): what was found and fixed while the
-- task was worked on, beyond its comments. Two kinds of entry are stored
-- here; the rest of the log (agent runs, pull requests, review-bot findings
-- and the verdicts on review comments) is read from where it already lives.
--
-- review: an adversarial review round autopilot ran in the session that
--   finished the task, before moving the session on. state: pending (asked
--   for, not yet typed), running (the prompt went in), done (the session
--   stopped after it: body is its report), skipped (never ran: the person
--   took the session, autopilot went off, the runner can't do it),
--   interrupted (a person prompted it mid-review), failed (the runner job
--   failed, or no Stop came back). found/fixed/rejected count the issues the
--   report names (NULL: not counted, or not countable); items lists them.
--   heads: the linked pull requests' head commits when it started and when
--   it was read, so a round that pushed fixes shows.
-- note: a line anyone working on the task added (wp worklog KEY -m), with an
--   optional tag (found, fixed, rejected) that counts it.
CREATE TABLE work_log (
    id          INTEGER PRIMARY KEY,
    issue_id    INTEGER NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
    kind        TEXT    NOT NULL,             -- review, note
    state       TEXT    NOT NULL DEFAULT '',  -- review states; '' for a note
    tag         TEXT    NOT NULL DEFAULT '',  -- a note's: found, fixed, rejected, ''
    session_id  INTEGER REFERENCES agent_sessions(id) ON DELETE SET NULL,
    run_id      INTEGER REFERENCES agent_runs(id) ON DELETE SET NULL,
    user_id     INTEGER REFERENCES users(id) ON DELETE SET NULL,
    agent       TEXT    NOT NULL DEFAULT '',
    prompt      TEXT    NOT NULL DEFAULT '',
    body        TEXT    NOT NULL DEFAULT '',
    reason      TEXT    NOT NULL DEFAULT '',
    found       INTEGER,
    fixed       INTEGER,
    rejected    INTEGER,
    items       TEXT    NOT NULL DEFAULT '[]',
    heads       TEXT    NOT NULL DEFAULT '{}',
    -- Counting a review's report: '' (nothing to count), pending, done,
    -- failed; read_by names the model, or "rules" when no model was needed.
    read_state  TEXT    NOT NULL DEFAULT '',
    read_by     TEXT    NOT NULL DEFAULT '',
    read_tries  INTEGER NOT NULL DEFAULT 0,
    read_at     INTEGER,
    started_at  INTEGER,
    ended_at    INTEGER,
    created_at  INTEGER NOT NULL,
    updated_at  INTEGER NOT NULL
);
CREATE INDEX work_log_issue ON work_log(issue_id, created_at);
CREATE INDEX work_log_session ON work_log(session_id, issue_id) WHERE kind = 'review';
CREATE INDEX work_log_live ON work_log(state) WHERE kind = 'review' AND state IN ('pending', 'running');
CREATE INDEX work_log_unread ON work_log(read_state, read_at) WHERE read_state = 'pending';

-- The review round a step runs before it moves its session on.
ALTER TABLE autopilot_steps ADD COLUMN review_id INTEGER REFERENCES work_log(id) ON DELETE SET NULL;
