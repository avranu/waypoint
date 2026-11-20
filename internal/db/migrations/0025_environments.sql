-- Environments and deployments (WP-D19): where a project's code runs, whether
-- it is up, and what version it runs.

CREATE TABLE environments (
    id          INTEGER PRIMARY KEY,
    project_id  INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    -- The attached repo its commits belong to (links, "behind its branch").
    repo_id     INTEGER REFERENCES project_repos(id) ON DELETE SET NULL,
    name        TEXT    NOT NULL, -- production, staging, a23x42s
    kind        TEXT    NOT NULL CHECK (kind IN ('production','staging','preview','dev')),
    url         TEXT    NOT NULL DEFAULT '',
    health_url  TEXT    NOT NULL DEFAULT '', -- '' = the url
    expected_status INTEGER NOT NULL DEFAULT 0, -- 0 = any 2xx
    interval_s  INTEGER NOT NULL DEFAULT 60,    -- 0 = don't check
    -- A version endpoint that reports what runs: a JSON document whose
    -- fields (dot paths like git.commit) hold the commit, branch and version.
    version_url   TEXT NOT NULL DEFAULT '',
    commit_field  TEXT NOT NULL DEFAULT '',
    branch_field  TEXT NOT NULL DEFAULT '',
    version_field TEXT NOT NULL DEFAULT '',
    -- Dev and preview environments come and go.
    ephemeral   INTEGER NOT NULL DEFAULT 0,
    ttl_s       INTEGER NOT NULL DEFAULT 0, -- 0 = none given
    expires_at  INTEGER,                    -- a lease: stopped then (set by up --ttl; deploys renew it)
    -- What it runs, from the latest successful or detected deployment.
    branch      TEXT    NOT NULL DEFAULT '',
    commit_sha  TEXT    NOT NULL DEFAULT '',
    version     TEXT    NOT NULL DEFAULT '',
    deployed_at INTEGER,
    -- What the last check said.
    status        TEXT    NOT NULL DEFAULT 'unknown' CHECK (status IN ('unknown','up','degraded','down')),
    status_since  INTEGER,
    latency_ms    INTEGER,
    http_code     INTEGER,
    error         TEXT    NOT NULL DEFAULT '',
    tls_expires_at INTEGER,
    fail_streak   INTEGER NOT NULL DEFAULT 0, -- consecutive failed checks (2 before "down")
    mismatch      INTEGER NOT NULL DEFAULT 0, -- reports a commit other than the last deployment's
    last_checked_at INTEGER,
    next_check_at   INTEGER,
    last_ok_at      INTEGER,
    position    INTEGER NOT NULL DEFAULT 0,
    created_by  INTEGER REFERENCES users(id) ON DELETE SET NULL,
    created_agent TEXT  NOT NULL DEFAULT '',
    created_at  INTEGER NOT NULL,
    updated_at  INTEGER NOT NULL,
    stopped_at  INTEGER,
    UNIQUE (project_id, name)
);
CREATE INDEX environments_due ON environments(next_check_at) WHERE stopped_at IS NULL AND interval_s > 0;
CREATE INDEX environments_repo ON environments(repo_id);

-- Minute samples, kept 48 hours. One row per minute: if a check ran more
-- than once in the minute, a failure wins.
CREATE TABLE environment_checks (
    environment_id INTEGER NOT NULL REFERENCES environments(id) ON DELETE CASCADE,
    at         INTEGER NOT NULL, -- the minute, ms since the epoch
    ok         INTEGER NOT NULL,
    latency_ms INTEGER NOT NULL DEFAULT 0,
    code       INTEGER NOT NULL DEFAULT 0,
    error      TEXT    NOT NULL DEFAULT '',
    PRIMARY KEY (environment_id, at)
) WITHOUT ROWID;

-- Hourly aggregates, kept 90 days; uptime per day sums these.
CREATE TABLE environment_hourly (
    environment_id INTEGER NOT NULL REFERENCES environments(id) ON DELETE CASCADE,
    hour       INTEGER NOT NULL, -- the hour, ms since the epoch
    checks     INTEGER NOT NULL,
    ok_checks  INTEGER NOT NULL,
    latency_sum INTEGER NOT NULL DEFAULT 0, -- of the ok checks
    latency_max INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (environment_id, hour)
) WITHOUT ROWID;

CREATE TABLE deployments (
    id             INTEGER PRIMARY KEY,
    environment_id INTEGER NOT NULL REFERENCES environments(id) ON DELETE CASCADE,
    commit_sha     TEXT    NOT NULL DEFAULT '',
    branch         TEXT    NOT NULL DEFAULT '',
    version        TEXT    NOT NULL DEFAULT '',
    status         TEXT    NOT NULL CHECK (status IN ('started','succeeded','failed','detected')),
    started_at     INTEGER NOT NULL,
    finished_at    INTEGER,
    -- Who: a person, an agent, CI (with the run's link) or the version
    -- endpoint ('detected').
    by_kind        TEXT    NOT NULL DEFAULT 'person' CHECK (by_kind IN ('person','agent','ci','detected')),
    by_user        INTEGER REFERENCES users(id) ON DELETE SET NULL,
    by_agent       TEXT    NOT NULL DEFAULT '',
    ci_url         TEXT    NOT NULL DEFAULT '',
    notes          TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX deployments_env ON deployments(environment_id, started_at DESC);
CREATE INDEX deployments_commit ON deployments(commit_sha) WHERE commit_sha != '';

-- The latest head of each branch, from push events, to say how far behind
-- an environment is.
CREATE TABLE branch_heads (
    repo      TEXT    NOT NULL COLLATE NOCASE, -- owner/name
    branch    TEXT    NOT NULL,
    sha       TEXT    NOT NULL,
    pushed_at INTEGER NOT NULL,
    PRIMARY KEY (repo, branch)
) WITHOUT ROWID;
