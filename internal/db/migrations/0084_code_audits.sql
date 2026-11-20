-- Audits (WP-348, WP-ADR20): an adversarial review a person, an agent or an
-- automation asks for, of a repository, a branch, a commit, a range or a
-- pull request. A model on the requester's runner (Grok 4.7 through Cursor
-- by default) reads the code read-only and reports findings; nothing is
-- fixed unless someone asks for a fix task. The findings are the model's
-- claims: they never feed the review gate or babysit.
CREATE TABLE code_audits (
    id              INTEGER PRIMARY KEY,
    project_id      INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    forge_id        INTEGER NOT NULL REFERENCES forges(id) ON DELETE CASCADE,
    repo            TEXT    NOT NULL,
    target          TEXT    NOT NULL,             -- repo, branch, commit, range, pr
    ref             TEXT    NOT NULL DEFAULT '',  -- what is checked out ('' : the default branch)
    base            TEXT    NOT NULL DEFAULT '',  -- what the change is read against ('' : the whole tree)
    pr_id           INTEGER REFERENCES pull_requests(id) ON DELETE SET NULL,
    pr_number       INTEGER NOT NULL DEFAULT 0,
    title           TEXT    NOT NULL DEFAULT '',  -- what was audited, in words
    instructions    TEXT    NOT NULL DEFAULT '',  -- what the requester asked to look for
    prompt          TEXT    NOT NULL DEFAULT '',  -- what the model is asked, built when it was requested
    models          TEXT    NOT NULL DEFAULT '[]', -- the chain: [{cli, model}], tried in order
    post_comment    INTEGER NOT NULL DEFAULT 0,   -- a pull request's audit is posted on it
    fix             INTEGER NOT NULL DEFAULT 0,   -- a task for Claude to check and fix the findings
    -- queued, running, done, failed, limited (a usage limit stopped every model), cancelled
    status          TEXT    NOT NULL DEFAULT 'queued',
    slot            INTEGER NOT NULL DEFAULT 0,   -- the chain entry being tried
    job_id          INTEGER REFERENCES runner_jobs(id) ON DELETE SET NULL,
    attempts        TEXT    NOT NULL DEFAULT '[]', -- [{model, job, outcome, error}]
    retries         INTEGER NOT NULL DEFAULT 0,   -- times the whole chain was busy and tried again
    retry_at        INTEGER,                      -- a queued audit waits until then
    model           TEXT    NOT NULL DEFAULT '',  -- the model that answered
    summary         TEXT    NOT NULL DEFAULT '',
    answer          TEXT    NOT NULL DEFAULT '',  -- the model's answer as it came
    error           TEXT    NOT NULL DEFAULT '',
    tokens          TEXT    NOT NULL DEFAULT '{}',
    comment_url     TEXT    NOT NULL DEFAULT '',
    fix_issue_id    INTEGER REFERENCES issues(id) ON DELETE SET NULL,
    owner_id        INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE, -- whose runner runs it
    requested_by    INTEGER REFERENCES users(id) ON DELETE SET NULL,
    requested_agent TEXT    NOT NULL DEFAULT '',  -- the agent that asked, if one did
    requested_via   TEXT    NOT NULL DEFAULT '',  -- cli, web, automation, api
    automation_id   INTEGER REFERENCES automations(id) ON DELETE SET NULL,
    session_id      INTEGER REFERENCES agent_sessions(id) ON DELETE SET NULL, -- told when it ends
    blocking        INTEGER NOT NULL DEFAULT 0,
    request_key     TEXT    NOT NULL DEFAULT '',  -- an automation step's, so a step run twice asks once
    created_at      INTEGER NOT NULL,
    started_at      INTEGER,
    finished_at     INTEGER,
    settled_at      INTEGER                       -- what its end sets off (comment, fix task, telling) is done
);
CREATE INDEX code_audits_project ON code_audits(project_id, id);
CREATE INDEX code_audits_pr ON code_audits(pr_id) WHERE pr_id IS NOT NULL;
CREATE UNIQUE INDEX code_audits_request ON code_audits(request_key) WHERE request_key != '';
CREATE INDEX code_audits_open ON code_audits(status) WHERE status IN ('queued','running');

CREATE TABLE code_audit_findings (
    id         INTEGER PRIMARY KEY,
    audit_id   INTEGER NOT NULL REFERENCES code_audits(id) ON DELETE CASCADE,
    ordinal    INTEGER NOT NULL,              -- 1, 2, …: the model's order, worst first
    title      TEXT    NOT NULL,
    path       TEXT    NOT NULL DEFAULT '',
    line       INTEGER NOT NULL DEFAULT 0,
    severity   TEXT    NOT NULL DEFAULT 'medium', -- critical, high, medium, low
    problem    TEXT    NOT NULL DEFAULT '',   -- what goes wrong, and when
    confirm    TEXT    NOT NULL DEFAULT '',   -- how someone could confirm it
    -- open (a claim), confirmed, fixed, rejected (wrong or not worth it)
    state      TEXT    NOT NULL DEFAULT 'open',
    note       TEXT    NOT NULL DEFAULT '',
    issue_id   INTEGER REFERENCES issues(id) ON DELETE SET NULL, -- filed for it, or that fixed it
    decided_by INTEGER REFERENCES users(id) ON DELETE SET NULL,
    decided_agent TEXT NOT NULL DEFAULT '',
    decided_at INTEGER,
    UNIQUE (audit_id, ordinal)
);
