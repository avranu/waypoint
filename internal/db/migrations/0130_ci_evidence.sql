-- CI evidence (WP-704). The backend test jobs print each test's result,
-- their merged cover profiles and the tree's #nosec and //nolint comments
-- as "wp-evidence" lines at the end of their log (internal/testevidence);
-- the CI history pass reads the logs of the jobs that carry them and keeps
-- what they say, so flaky and slow tests, coverage per commit and accepted
-- findings can be asked about later.

-- Whether a finished job's log was read for evidence: NULL not yet, 1 kept,
-- 0 none in it, -1 unreadable (gone, cut, garbled), -2 not read (a job of
-- a kind that never carries any, see ci_evidence_kinds).
ALTER TABLE ci_jobs ADD COLUMN evidence INTEGER;
CREATE INDEX ci_jobs_evidence ON ci_jobs(ended_at) WHERE evidence IS NULL;

-- Which jobs carry evidence, learned per repo, workflow and job name with
-- its numbers as N ("backend shard (N)"): a kind whose last few logs had
-- none is read again only once a week.
CREATE TABLE ci_evidence_kinds (
    forge_id   INTEGER NOT NULL REFERENCES forges(id) ON DELETE CASCADE,
    repo       TEXT    NOT NULL,
    workflow   TEXT    NOT NULL,
    kind       TEXT    NOT NULL,
    kept       INTEGER NOT NULL DEFAULT 0, -- logs with evidence
    misses     INTEGER NOT NULL DEFAULT 0, -- logs without, since the last with
    read_at    INTEGER NOT NULL,
    PRIMARY KEY (forge_id, repo, workflow, kind)
);

-- A test, by package (the module's path cut) and top-level name.
CREATE TABLE ci_tests (
    id       INTEGER PRIMARY KEY,
    forge_id INTEGER NOT NULL REFERENCES forges(id) ON DELETE CASCADE,
    repo     TEXT    NOT NULL,
    package  TEXT    NOT NULL,
    name     TEXT    NOT NULL,
    UNIQUE (forge_id, repo, package, name)
);

-- One test's result in one job. Kept a month: a test that both passed and
-- failed on one commit in one tier is flaky.
CREATE TABLE ci_test_runs (
    test_id INTEGER NOT NULL REFERENCES ci_tests(id) ON DELETE CASCADE,
    task_id INTEGER NOT NULL,
    sha     TEXT    NOT NULL,
    ref     TEXT    NOT NULL DEFAULT '',
    event   TEXT    NOT NULL DEFAULT '',
    tier    TEXT    NOT NULL DEFAULT '', -- draft, or '' for a full run
    on_default INTEGER NOT NULL DEFAULT 0, -- a push to the default branch
    outcome TEXT    NOT NULL,            -- pass, fail, skip
    ms      INTEGER NOT NULL,
    at      INTEGER NOT NULL,            -- the job's end
    PRIMARY KEY (test_id, task_id)
);
CREATE INDEX ci_test_runs_at ON ci_test_runs(at);
CREATE INDEX ci_test_runs_default ON ci_test_runs(test_id, at) WHERE on_default = 1;

-- A commit's coverage, the shards' profiles merged: files is gzipped JSON of
-- testevidence.File (path, statements per block, a bit per block that ran).
-- shards lists the "I/N" read so far; complete once they cover the tests.
CREATE TABLE ci_coverage (
    forge_id   INTEGER NOT NULL REFERENCES forges(id) ON DELETE CASCADE,
    repo       TEXT    NOT NULL,
    sha        TEXT    NOT NULL,
    ref        TEXT    NOT NULL DEFAULT '',
    event      TEXT    NOT NULL DEFAULT '',
    on_default INTEGER NOT NULL DEFAULT 0, -- a push to the default branch
    shards     TEXT    NOT NULL DEFAULT '',
    complete   INTEGER NOT NULL DEFAULT 0,
    files      BLOB,                          -- NULL once older than a month
    stmts      INTEGER NOT NULL DEFAULT 0,
    covered    INTEGER NOT NULL DEFAULT 0,
    updated_at INTEGER NOT NULL,
    PRIMARY KEY (forge_id, repo, sha)
);
CREATE INDEX ci_coverage_updated ON ci_coverage(forge_id, repo, on_default, updated_at);

-- The same per package (directory), kept for trends.
CREATE TABLE ci_coverage_pkgs (
    forge_id INTEGER NOT NULL REFERENCES forges(id) ON DELETE CASCADE,
    repo     TEXT    NOT NULL,
    sha      TEXT    NOT NULL,
    package  TEXT    NOT NULL,
    stmts    INTEGER NOT NULL,
    covered  INTEGER NOT NULL,
    PRIMARY KEY (forge_id, repo, sha, package)
);

-- Each scan of the default branch's #nosec and //nolint comments.
CREATE TABLE ci_suppression_scans (
    forge_id INTEGER NOT NULL REFERENCES forges(id) ON DELETE CASCADE,
    repo     TEXT    NOT NULL,
    sha      TEXT    NOT NULL,
    at       INTEGER NOT NULL,
    count    INTEGER NOT NULL,
    PRIMARY KEY (forge_id, repo, sha)
);

-- The suppressions of the default branch, as a ledger: fingerprint is the
-- kind, rules, file and squeezed code line (and which of identical ones),
-- so a suppression keeps its row when lines above it move. gone_at is set
-- when a later scan no longer has it.
CREATE TABLE ci_suppressions (
    forge_id    INTEGER NOT NULL REFERENCES forges(id) ON DELETE CASCADE,
    repo        TEXT    NOT NULL,
    fingerprint TEXT    NOT NULL,
    kind        TEXT    NOT NULL, -- nosec, nolint
    rules       TEXT    NOT NULL DEFAULT '', -- comma separated; '' is every rule
    path        TEXT    NOT NULL,
    line        INTEGER NOT NULL,
    code        TEXT    NOT NULL DEFAULT '',
    reason      TEXT    NOT NULL DEFAULT '',
    first_sha   TEXT    NOT NULL,
    first_at    INTEGER NOT NULL,
    last_sha    TEXT    NOT NULL,
    last_at     INTEGER NOT NULL,
    gone_at     INTEGER,
    PRIMARY KEY (forge_id, repo, fingerprint)
);
