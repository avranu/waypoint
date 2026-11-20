-- Test evidence (WP-715): what wp testsum recorded on the exact tree a commit
-- shipped, one row per commit, sent by wp ship. runs is JSON of the runs on
-- that tree (command, counts, packages, failures); nearest the newest run on
-- another tree when none matched. Reported by the CLI: a fact check for
-- people and agents reading a task, not something a gate may rely on.
CREATE TABLE test_evidence (
    id         INTEGER PRIMARY KEY,
    project_id INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    issue_id   INTEGER REFERENCES issues(id) ON DELETE SET NULL,
    repo       TEXT    NOT NULL COLLATE NOCASE,
    sha        TEXT    NOT NULL,
    tree       TEXT    NOT NULL,
    outcome    TEXT    NOT NULL,             -- pass, fail, empty, none
    line       TEXT    NOT NULL,
    runs       TEXT    NOT NULL DEFAULT '[]',
    nearest    TEXT    NOT NULL DEFAULT '',
    session_id INTEGER REFERENCES agent_sessions(id) ON DELETE SET NULL,
    user_id    INTEGER REFERENCES users(id) ON DELETE SET NULL,
    agent      TEXT    NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    UNIQUE (project_id, repo, sha)
);
CREATE INDEX test_evidence_issue ON test_evidence(issue_id, updated_at);
CREATE INDEX test_evidence_repo_sha ON test_evidence(repo COLLATE NOCASE, sha);
CREATE INDEX test_evidence_week ON test_evidence(project_id, created_at);
