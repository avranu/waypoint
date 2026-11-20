-- Merge gates (WP-D21): findings that know their forge threads and states,
-- the review gate, CI results, branch activity and merge when ready.

-- A repo's review gate: Waypoint posts a `waypoint/review` status on its
-- pull requests, failing while findings at or above gate_severity are open.
ALTER TABLE project_repos ADD COLUMN review_gate INTEGER NOT NULL DEFAULT 0;
ALTER TABLE project_repos ADD COLUMN gate_severity TEXT NOT NULL DEFAULT 'medium'; -- critical, high, medium, low

-- Findings keep their comment and thread on the forge, where they were
-- raised and last checked, and whether they sit on a private path (their
-- text stays off anything a forge copies to GitHub).
-- State: open, fixed, withdrawn, dismissed, resolved (a person resolved the
-- thread on the forge).
ALTER TABLE bot_findings ADD COLUMN thread_id   TEXT NOT NULL DEFAULT '';
ALTER TABLE bot_findings ADD COLUMN ordinal     INTEGER NOT NULL DEFAULT 0; -- its place among the run's findings: the comment's marker names run and place
ALTER TABLE bot_findings ADD COLUMN comment_id  TEXT NOT NULL DEFAULT '';
ALTER TABLE bot_findings ADD COLUMN head_sha    TEXT NOT NULL DEFAULT '';
ALTER TABLE bot_findings ADD COLUMN checked_sha TEXT NOT NULL DEFAULT '';
ALTER TABLE bot_findings ADD COLUMN private     INTEGER NOT NULL DEFAULT 0;
ALTER TABLE bot_findings ADD COLUMN replies_seen INTEGER NOT NULL DEFAULT 0; -- comments in its thread already answered or read
ALTER TABLE bot_findings ADD COLUMN updated_at  INTEGER NOT NULL DEFAULT 0;
CREATE INDEX bot_findings_thread ON bot_findings(pr_id, thread_id) WHERE thread_id != '';
UPDATE bot_findings SET updated_at = created_at;
UPDATE bot_findings SET head_sha = coalesce((SELECT head_sha FROM bot_runs r WHERE r.id = bot_findings.run_id), '');

-- How a finding's state changed, and who or what changed it.
CREATE TABLE finding_events (
    id         INTEGER PRIMARY KEY,
    finding_id INTEGER NOT NULL REFERENCES bot_findings(id) ON DELETE CASCADE,
    pr_id      INTEGER NOT NULL REFERENCES pull_requests(id) ON DELETE CASCADE,
    old_state  TEXT    NOT NULL DEFAULT '',
    state      TEXT    NOT NULL,
    by_kind    TEXT    NOT NULL, -- person, forge, verify, reply, agent, system
    by_name    TEXT    NOT NULL DEFAULT '',
    user_id    INTEGER REFERENCES users(id) ON DELETE SET NULL,
    sha        TEXT    NOT NULL DEFAULT '',
    note       TEXT    NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL
);
CREATE INDEX finding_events_finding ON finding_events(finding_id, id);
CREATE INDEX finding_events_pr ON finding_events(pr_id, id);

-- The gate, CI and merge state of a pull request.
ALTER TABLE pull_requests ADD COLUMN mergeable INTEGER;                       -- the forge's word; NULL when unknown
ALTER TABLE pull_requests ADD COLUMN gate_state TEXT NOT NULL DEFAULT '';     -- the last status posted: pending, success, failure
ALTER TABLE pull_requests ADD COLUMN gate_note TEXT NOT NULL DEFAULT '';
ALTER TABLE pull_requests ADD COLUMN gate_sha TEXT NOT NULL DEFAULT '';       -- the head it was posted on
ALTER TABLE pull_requests ADD COLUMN gate_open INTEGER NOT NULL DEFAULT 0;    -- findings blocking at that time
ALTER TABLE pull_requests ADD COLUMN gate_checked_at INTEGER NOT NULL DEFAULT 0;
ALTER TABLE pull_requests ADD COLUMN ci_state TEXT NOT NULL DEFAULT '';       -- pending, passed, failed, none; '' unknown
ALTER TABLE pull_requests ADD COLUMN ci_sha TEXT NOT NULL DEFAULT '';
ALTER TABLE pull_requests ADD COLUMN ci_announced_sha TEXT NOT NULL DEFAULT ''; -- the head pr.ci was emitted for
ALTER TABLE pull_requests ADD COLUMN armed_at INTEGER;                        -- merge when ready
ALTER TABLE pull_requests ADD COLUMN armed_by INTEGER REFERENCES users(id) ON DELETE SET NULL;
ALTER TABLE pull_requests ADD COLUMN armed_agent TEXT NOT NULL DEFAULT '';
ALTER TABLE pull_requests ADD COLUMN armed_style TEXT NOT NULL DEFAULT '';    -- '' is the repo's default
ALTER TABLE pull_requests ADD COLUMN armed_label INTEGER NOT NULL DEFAULT 0;  -- 1: armed by the automerge label; 2: the label is there but a person took the arming off
ALTER TABLE pull_requests ADD COLUMN armed_note TEXT NOT NULL DEFAULT '';     -- why it hasn't merged yet, when it tried
CREATE INDEX pull_requests_armed ON pull_requests(armed_at) WHERE armed_at IS NOT NULL;
CREATE INDEX pull_requests_gate ON pull_requests(gate_state) WHERE gate_state = 'failure';

-- Statuses and checks on a commit of an attached repo, as last read. A head
-- is polled every minute while any check is pending, for two hours.
CREATE TABLE ci_heads (
    id         INTEGER PRIMARY KEY,
    forge_id   INTEGER NOT NULL REFERENCES forges(id) ON DELETE CASCADE,
    repo       TEXT    NOT NULL,
    sha        TEXT    NOT NULL,
    state      TEXT    NOT NULL DEFAULT 'pending', -- pending, passed, failed, none
    seen_at    INTEGER NOT NULL,                   -- when Waypoint first saw this head
    checked_at INTEGER NOT NULL DEFAULT 0,
    settled_at INTEGER,
    UNIQUE (forge_id, repo, sha)
);
CREATE INDEX ci_heads_pending ON ci_heads(state, checked_at) WHERE state = 'pending';

CREATE TABLE ci_checks (
    head_id     INTEGER NOT NULL REFERENCES ci_heads(id) ON DELETE CASCADE,
    name        TEXT    NOT NULL,
    state       TEXT    NOT NULL, -- pending, success, failed
    url         TEXT    NOT NULL DEFAULT '',
    description TEXT    NOT NULL DEFAULT '',
    source      TEXT    NOT NULL DEFAULT '',
    job_id      TEXT    NOT NULL DEFAULT '',
    log         TEXT    NOT NULL DEFAULT '', -- the tail of a failed job's log (64 KB)
    updated_at  INTEGER NOT NULL,
    PRIMARY KEY (head_id, name)
);

-- The last push to each branch of an attached repo: branches in work.
CREATE TABLE branch_activity (
    id         INTEGER PRIMARY KEY,
    forge_id   INTEGER NOT NULL REFERENCES forges(id) ON DELETE CASCADE,
    repo       TEXT    NOT NULL,
    branch     TEXT    NOT NULL,
    head_sha   TEXT    NOT NULL,
    before_sha TEXT    NOT NULL DEFAULT '',
    pusher     TEXT    NOT NULL DEFAULT '',
    commits    INTEGER NOT NULL DEFAULT 0,
    created    INTEGER NOT NULL DEFAULT 0, -- the push created the branch
    pushed_at  INTEGER NOT NULL,
    UNIQUE (forge_id, repo, branch)
);
CREATE INDEX branch_activity_time ON branch_activity(pushed_at);

-- Every project answers replies to its bots, as new projects do (WP-D21;
-- the rule matches core.AutomationTemplates).
INSERT INTO automations(project_id, name, enabled, rule, position, created_at, updated_at)
SELECT id, 'Answer replies', 1,
       '{"trigger":{"event":"pr_comment"},"actions":[{"type":"reply"}]}',
       102, CAST(strftime('%s','now') AS INTEGER) * 1000, CAST(strftime('%s','now') AS INTEGER) * 1000
FROM projects;
