-- Review triage (WP-692): what wp ship was told about the change as its
-- adversarial review was about to be scheduled, one row per round. The
-- outcomes (what the review found, findings on the pull request, escapes)
-- are read from their own tables when asked for, so a row is the features
-- and the decision only.
CREATE TABLE review_triage (
    id         INTEGER PRIMARY KEY,
    round_id   INTEGER REFERENCES work_log(id) ON DELETE SET NULL,
    issue_id   INTEGER NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
    project_id INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    repo       TEXT    NOT NULL DEFAULT '',
    head       TEXT    NOT NULL DEFAULT '',
    base       TEXT    NOT NULL DEFAULT '',
    rule       TEXT    NOT NULL,             -- the floor or rule that decided
    would_skip INTEGER NOT NULL DEFAULT 0,   -- 1: a rule would skip this, active or not
    action     TEXT    NOT NULL,             -- review or skip
    why        TEXT    NOT NULL DEFAULT '',  -- why the action, when would_skip doesn't say
    version    TEXT    NOT NULL DEFAULT '',  -- the rules that decided
    features   TEXT    NOT NULL DEFAULT '{}',
    created_at INTEGER NOT NULL
);
CREATE INDEX review_triage_project ON review_triage(project_id, created_at);
CREATE INDEX review_triage_round ON review_triage(round_id);
CREATE INDEX review_triage_issue ON review_triage(issue_id);
