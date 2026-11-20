-- Tasks an agent files from another repo (WP-378). An agent working in
-- one repo knows little of another, and tends to push work off to it; the
-- agents there are told so when they take or read such a task, can decline
-- it with a reason, and the decline (or the task being finished) goes back
-- to whoever on the filing side still cares, without a task loop.
--
-- cross_filed: one row per task an agent filed while its session worked in
-- another project (or a repo attached to none). told is the outcome last
-- passed on (declined, canceled, done), so news goes once.
CREATE TABLE cross_filed (
    issue_id     INTEGER PRIMARY KEY REFERENCES issues(id) ON DELETE CASCADE,
    project_id   INTEGER REFERENCES projects(id) ON DELETE SET NULL,
    repo         TEXT    NOT NULL DEFAULT '',
    session_id   INTEGER REFERENCES agent_sessions(id) ON DELETE SET NULL,
    agent        TEXT    NOT NULL DEFAULT '',
    created_at   INTEGER NOT NULL,
    declined_at  INTEGER,
    declined_by  TEXT    NOT NULL DEFAULT '',
    declined_why TEXT    NOT NULL DEFAULT '',
    told         TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX cross_filed_project ON cross_filed(project_id);

-- session_issue_refs: the sessions outside a cross-filed task's project
-- that showed they care about it: filed it, read it (wp show, wp next KEY)
-- or named it in a prompt or a final message. They hear how it ends.
CREATE TABLE session_issue_refs (
    session_id INTEGER NOT NULL REFERENCES agent_sessions(id) ON DELETE CASCADE,
    issue_id   INTEGER NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
    how        TEXT    NOT NULL, -- filed, read, named
    at         INTEGER NOT NULL,
    PRIMARY KEY (session_id, issue_id)
);
CREATE INDEX session_issue_refs_issue ON session_issue_refs(issue_id);

-- project_notices: news for the next agent session that starts a turn in a
-- project, when no session there that cared is still running (a declined
-- task whose filer is gone). Taken once; dropped when it expires.
CREATE TABLE project_notices (
    id           INTEGER PRIMARY KEY,
    project_id   INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    issue_id     INTEGER REFERENCES issues(id) ON DELETE CASCADE,
    body         TEXT    NOT NULL,
    created_at   INTEGER NOT NULL,
    expires_at   INTEGER NOT NULL,
    session_id   INTEGER REFERENCES agent_sessions(id) ON DELETE SET NULL,
    delivered_at INTEGER
);
CREATE INDEX project_notices_open ON project_notices(project_id, delivered_at);
