-- Runners (WP-D17): `wp runner` on a person's machine, and the work it does
-- there. A runner belongs to the person who registered it; only their jobs
-- run on it.
CREATE TABLE runners (
    id           INTEGER PRIMARY KEY,
    owner_id     INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name         TEXT    NOT NULL,                -- the hostname unless --name
    hostname     TEXT    NOT NULL DEFAULT '',
    os           TEXT    NOT NULL DEFAULT '',
    arch         TEXT    NOT NULL DEFAULT '',
    version      TEXT    NOT NULL DEFAULT '',     -- wp version
    labels       TEXT    NOT NULL DEFAULT '[]',
    allowed_dirs TEXT    NOT NULL DEFAULT '[]',   -- where command and apply jobs may run
    clis         TEXT    NOT NULL DEFAULT '[]',   -- agent CLIs found: [{name, version, signed_in, detail}]
    max_jobs     INTEGER NOT NULL DEFAULT 1,
    online       INTEGER NOT NULL DEFAULT 0,      -- as last announced, so a change is announced once
    last_seen_at INTEGER,                         -- last poll; online = within 90 s
    created_at   INTEGER NOT NULL,
    UNIQUE (owner_id, name)
);

CREATE TABLE runner_jobs (
    id               INTEGER PRIMARY KEY,
    owner_id         INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE, -- whose runners may take it
    created_by       INTEGER REFERENCES users(id) ON DELETE SET NULL,
    target_runner_id INTEGER REFERENCES runners(id) ON DELETE SET NULL,       -- a specific runner, or any of the owner's
    label            TEXT    NOT NULL DEFAULT '',  -- … with this label
    kind             TEXT    NOT NULL,             -- command, apply, agent, review
    title            TEXT    NOT NULL DEFAULT '',  -- one line for lists
    dir              TEXT    NOT NULL DEFAULT '',  -- command and apply: where it runs, for matching runners
    cli              TEXT    NOT NULL DEFAULT '',  -- agent and review: which CLI it needs
    input            TEXT    NOT NULL DEFAULT '{}',
    state            TEXT    NOT NULL DEFAULT 'queued', -- queued, running, done, failed, cancelled, expired
    runner_id        INTEGER REFERENCES runners(id) ON DELETE SET NULL,        -- the runner that took it
    runner_name      TEXT    NOT NULL DEFAULT '',
    timeout          INTEGER NOT NULL,             -- seconds
    attempts         INTEGER NOT NULL DEFAULT 0,
    cancel_requested INTEGER NOT NULL DEFAULT 0,
    exit_code        INTEGER,
    result           TEXT,                         -- JSON
    error            TEXT    NOT NULL DEFAULT '',
    log_size         INTEGER NOT NULL DEFAULT 0,   -- bytes in runner_job_logs
    source           TEXT    NOT NULL DEFAULT '',  -- request_cell, automation_step, bot_run, manual
    source_id        INTEGER NOT NULL DEFAULT 0,
    created_at       INTEGER NOT NULL,
    started_at       INTEGER,
    finished_at      INTEGER
);
CREATE INDEX runner_jobs_queue ON runner_jobs(owner_id, state, id);
CREATE INDEX runner_jobs_runner ON runner_jobs(runner_id, id);
CREATE INDEX runner_jobs_owner ON runner_jobs(owner_id, id);
CREATE INDEX runner_jobs_creator ON runner_jobs(created_by, id);
CREATE INDEX runner_jobs_source ON runner_jobs(source, source_id);
CREATE INDEX runner_jobs_state ON runner_jobs(state, created_at);

-- Output, apart from the job so lists never read it (capped at 1 MB).
CREATE TABLE runner_job_logs (
    job_id INTEGER PRIMARY KEY REFERENCES runner_jobs(id) ON DELETE CASCADE,
    text   TEXT NOT NULL DEFAULT ''
);
