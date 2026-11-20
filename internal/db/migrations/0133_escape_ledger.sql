-- The escape ledger (WP-705, WP-ADR68): every bug task's fix traced to what
-- introduced it, how it was found and how long it lived.
--
-- A runner with a checkout traces a merged pull request linked to a bug task
-- (internal/codeindex TraceFix: SZZ) and uploads one fix_traces row with its
-- candidate introducing changes and the files it fixed. The server keeps an
-- escapes row per bug task and fix: tasks without a trace are never guessed
-- at, and fix-worded commit subjects don't count (bug-task links only).

-- A fix pull request traced once. method: 'szz' (blamed the lines it removes
-- or changes), 'context' (only added lines; blamed their neighbours, weak) or
-- 'none' (note says why there is no introducer). An empty fix_sha means the
-- fix couldn't be placed in the checkout: it is due again once the pull
-- request's merge_sha is no longer tried_merge, the one the runner looked for.
CREATE TABLE fix_traces (
    pr_id     INTEGER PRIMARY KEY REFERENCES pull_requests(id) ON DELETE CASCADE,
    fix_sha   TEXT    NOT NULL DEFAULT '',
    tried_merge TEXT  NOT NULL DEFAULT '',
    landed_at INTEGER NOT NULL DEFAULT 0, -- when the fix reached its base
    method    TEXT    NOT NULL,
    note      TEXT    NOT NULL DEFAULT '',
    lines     INTEGER NOT NULL DEFAULT 0, -- lines blamed
    dropped   INTEGER NOT NULL DEFAULT 0, -- of those, on changes too big or too late to be the cause
    tags      TEXT    NOT NULL DEFAULT '[]',
    machine   TEXT    NOT NULL DEFAULT '',
    traced_at INTEGER NOT NULL
);

-- A candidate: the change that carried blamed lines to the base (a merge, or
-- the commit itself when it was pushed or squashed), its pull request when
-- known, and its share of the blamed lines.
CREATE TABLE fix_trace_introducers (
    pr_id        INTEGER NOT NULL REFERENCES fix_traces(pr_id) ON DELETE CASCADE,
    merge_sha    TEXT    NOT NULL,
    commit_sha   TEXT    NOT NULL DEFAULT '', -- the commit with the most blamed lines
    intro_pr_id  INTEGER REFERENCES pull_requests(id) ON DELETE SET NULL,
    intro_number INTEGER NOT NULL DEFAULT 0,
    lines        INTEGER NOT NULL,
    share        REAL    NOT NULL,
    confidence   REAL    NOT NULL, -- share, discounted for a weaker method
    at           INTEGER NOT NULL, -- when it reached the base
    PRIMARY KEY (pr_id, merge_sha)
);
CREATE INDEX fix_trace_introducers_intro ON fix_trace_introducers(intro_pr_id) WHERE intro_pr_id IS NOT NULL;

-- The code files a fix changed (not tests or docs), for risk per file.
CREATE TABLE fix_trace_files (
    pr_id INTEGER NOT NULL REFERENCES fix_traces(pr_id) ON DELETE CASCADE,
    path  TEXT    NOT NULL,
    lines INTEGER NOT NULL,
    PRIMARY KEY (pr_id, path)
);
CREATE INDEX fix_trace_files_path ON fix_trace_files(path);

-- One bug task fixed by one pull request. stage and detector come from what
-- filed the task (labels, the automation, who wrote it) and, for what an
-- agent wrote, its words. The best introducer is copied here so the ledger
-- reads without a join; 0 / NULL when the trace found none.
CREATE TABLE escapes (
    id          INTEGER PRIMARY KEY,
    issue_id    INTEGER NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
    pr_id       INTEGER NOT NULL REFERENCES pull_requests(id) ON DELETE CASCADE,
    project_id  INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    stage       TEXT    NOT NULL,
    detector    TEXT    NOT NULL,
    intro_merge TEXT    NOT NULL DEFAULT '',
    intro_pr_id INTEGER REFERENCES pull_requests(id) ON DELETE SET NULL,
    intro_at    INTEGER,
    confidence  REAL    NOT NULL DEFAULT 0,
    days_lived  REAL,
    fixed_at    INTEGER NOT NULL,
    created_at  INTEGER NOT NULL,
    UNIQUE (issue_id, pr_id)
);
CREATE INDEX escapes_project ON escapes(project_id, fixed_at);
CREATE INDEX escapes_pr ON escapes(pr_id);
CREATE INDEX escapes_intro ON escapes(intro_pr_id) WHERE intro_pr_id IS NOT NULL;
