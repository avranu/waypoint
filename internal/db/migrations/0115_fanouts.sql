-- Fan-outs (WP-148, X6): a plan of work packages that run as budgeted task
-- sessions, a few at a time, instead of ad-hoc agents in one session. The
-- coordinator is the session that filed the plan; its machine and directory
-- are where the packages' worktrees and sessions start.
CREATE TABLE fanouts (
    id           INTEGER PRIMARY KEY,
    user_id      INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    project_id   INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    session_id   INTEGER REFERENCES agent_sessions(id) ON DELETE SET NULL, -- the coordinator
    agent        TEXT    NOT NULL DEFAULT '',
    machine      TEXT    NOT NULL DEFAULT '',
    dir          TEXT    NOT NULL DEFAULT '', -- the repo the worktrees come from
    base         TEXT    NOT NULL DEFAULT '', -- what each worktree starts from
    parallel     INTEGER NOT NULL DEFAULT 3,
    estimate_usd REAL    NOT NULL DEFAULT 0,
    week_pct     REAL    NOT NULL DEFAULT 0, -- the estimate as a share of a week's quota
    basis        TEXT    NOT NULL DEFAULT '', -- how the estimate was made, in words
    state        TEXT    NOT NULL DEFAULT 'running', -- asked, running, finished, stopped, declined
    request_id   INTEGER REFERENCES requests(id) ON DELETE SET NULL, -- the ask, over the line
    told         TEXT    NOT NULL DEFAULT '', -- the last news sent to the person instead of the coordinator
    created_at   INTEGER NOT NULL,
    ended_at     INTEGER
);
CREATE INDEX fanouts_user ON fanouts(user_id, created_at);
CREATE INDEX fanouts_state ON fanouts(state);

-- One package: its issue, model, budget and branch, and how it went.
-- state: queued, starting (claimed, being launched), running, done,
-- stopped (budget, or its session ended without wp done), failed (didn't
-- start), cancelled (wp fanout stop before it started).
CREATE TABLE fanout_items (
    id           INTEGER PRIMARY KEY,
    fanout_id    INTEGER NOT NULL REFERENCES fanouts(id) ON DELETE CASCADE,
    issue_id     INTEGER NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
    pos          INTEGER NOT NULL,
    model        TEXT    NOT NULL,
    budget_usd   REAL    NOT NULL,
    estimate_usd REAL    NOT NULL DEFAULT 0,
    branch       TEXT    NOT NULL,
    state        TEXT    NOT NULL DEFAULT 'queued',
    attach_id    TEXT    NOT NULL DEFAULT '',
    spent_usd    REAL    NOT NULL DEFAULT 0,
    note         TEXT    NOT NULL DEFAULT '',
    told         INTEGER NOT NULL DEFAULT 0, -- the coordinator heard why it waits
    started_at   INTEGER,
    ended_at     INTEGER,
    UNIQUE (fanout_id, pos)
);
CREATE INDEX fanout_items_issue ON fanout_items(issue_id, state);
