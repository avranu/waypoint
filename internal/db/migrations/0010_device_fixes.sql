-- Usage reports carry an id, so a hook that retries after a lost response
-- doesn't count the same tokens twice.
CREATE TABLE agent_usage_reports (
    issue_id   INTEGER NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
    agent      TEXT NOT NULL,
    report_id  TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    PRIMARY KEY (issue_id, agent, report_id)
) WITHOUT ROWID;

-- CLI tokens are now made when the CLI collects them, never stored here;
-- drop any left from before.
UPDATE device_logins SET token = '' WHERE token != '';
