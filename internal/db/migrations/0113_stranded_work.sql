-- Stranded work (WP-567): a session cleared or ended mid-task leaves its
-- changes in a checkout. Each Stop records where the session's work stands
-- (its checkouts' git state and the background commands still running);
-- when the session ends with the task unfinished, the task goes back to
-- To Do and a row here says where to resume it.
ALTER TABLE agent_sessions ADD COLUMN work_state TEXT NOT NULL DEFAULT '';

CREATE TABLE stranded_work (
    id         INTEGER PRIMARY KEY,
    issue_id   INTEGER NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
    session_id INTEGER REFERENCES agent_sessions(id) ON DELETE SET NULL,
    agent      TEXT    NOT NULL DEFAULT '',
    machine    TEXT    NOT NULL DEFAULT '',
    top        TEXT    NOT NULL DEFAULT '', -- the checkout's top directory
    branch     TEXT    NOT NULL DEFAULT '',
    files      TEXT    NOT NULL DEFAULT '[]', -- JSON: git status --porcelain lines (at most 20)
    file_count INTEGER NOT NULL DEFAULT 0,
    unpushed   INTEGER NOT NULL DEFAULT 0, -- commits no remote branch has
    background TEXT    NOT NULL DEFAULT '[]', -- JSON: background commands that died with the session
    why        TEXT    NOT NULL DEFAULT '', -- clear, exit, logout, other, idle (the hourly sweep)
    at         INTEGER NOT NULL
);
CREATE INDEX stranded_work_issue ON stranded_work(issue_id, at);
CREATE INDEX stranded_work_top ON stranded_work(machine, top);
