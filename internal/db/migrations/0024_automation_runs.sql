-- waypoint:no-foreign-keys
-- Automations that run in steps (WP-D18): admin automations outside any
-- project, a record of every run with its steps, durable runs that wait for
-- events or time, and the state counts and system checks keep.

-- automations.project_id may now be NULL (an admin automation: it sees every
-- project's events and the system's), and a webhook trigger has a secret
-- token. SQLite can't relax NOT NULL in place, so rebuild the table.
CREATE TABLE automations_new (
    id          INTEGER PRIMARY KEY,
    project_id  INTEGER REFERENCES projects(id) ON DELETE CASCADE, -- NULL: admin
    name        TEXT    NOT NULL,
    enabled     INTEGER NOT NULL DEFAULT 1,
    rule        TEXT    NOT NULL,
    position    INTEGER NOT NULL DEFAULT 0,
    run_count   INTEGER NOT NULL DEFAULT 0,
    last_run_at INTEGER,
    last_error  TEXT    NOT NULL DEFAULT '',
    created_by  INTEGER REFERENCES users(id) ON DELETE SET NULL,
    created_at  INTEGER NOT NULL,
    updated_at  INTEGER NOT NULL,
    hook_token  TEXT    NOT NULL DEFAULT '' -- path secret of the incoming webhook trigger
);
INSERT INTO automations_new(id, project_id, name, enabled, rule, position, run_count, last_run_at, last_error, created_by, created_at, updated_at)
    SELECT id, project_id, name, enabled, rule, position, run_count, last_run_at, last_error, created_by, created_at, updated_at FROM automations;
DROP TABLE automations;
ALTER TABLE automations_new RENAME TO automations;
CREATE INDEX automations_project ON automations(project_id);
CREATE UNIQUE INDEX automations_hook ON automations(hook_token) WHERE hook_token != '';

-- Rules as they were when runs used them, shared by every run of the same
-- text, so a run shows what actually ran after the automation was edited.
CREATE TABLE automation_rules (
    id            INTEGER PRIMARY KEY,
    automation_id INTEGER NOT NULL REFERENCES automations(id) ON DELETE CASCADE,
    hash          TEXT    NOT NULL,
    rule          TEXT    NOT NULL,
    created_at    INTEGER NOT NULL,
    UNIQUE (automation_id, hash)
);

-- One row per firing.
CREATE TABLE automation_runs (
    id            INTEGER PRIMARY KEY,
    automation_id INTEGER NOT NULL REFERENCES automations(id) ON DELETE CASCADE,
    project_id    INTEGER,                       -- NULL: an admin automation
    rule_id       INTEGER NOT NULL REFERENCES automation_rules(id),
    status        TEXT    NOT NULL,              -- queued, running, waiting, succeeded, failed, cancelled, skipped
    via           TEXT    NOT NULL DEFAULT 'event', -- event, schedule, manual, api, cli, webhook, rerun
    trigger_name  TEXT    NOT NULL DEFAULT '',
    summary       TEXT    NOT NULL DEFAULT '',   -- the trigger in words: "robin moved WP-4 to Done"
    event         TEXT    NOT NULL DEFAULT '{}', -- the event, as a snapshot (JSON)
    inputs        TEXT    NOT NULL DEFAULT '{}', -- values given to a manual run (JSON)
    actor_id      INTEGER,                       -- the person whose change or click started it
    agent         TEXT    NOT NULL DEFAULT '',
    depth         INTEGER NOT NULL DEFAULT 0,    -- loop guard: how many automations deep
    durable       INTEGER NOT NULL DEFAULT 0,    -- carried on by the worker, across restarts
    concurrency   TEXT    NOT NULL DEFAULT 'parallel',
    rerun_of      INTEGER,
    error         TEXT    NOT NULL DEFAULT '',
    attempts      INTEGER NOT NULL DEFAULT 0,
    not_before    INTEGER NOT NULL DEFAULT 0,
    created_at    INTEGER NOT NULL,
    started_at    INTEGER,
    finished_at   INTEGER
);
CREATE INDEX automation_runs_automation ON automation_runs(automation_id, id);
CREATE INDEX automation_runs_project ON automation_runs(project_id, id);
CREATE INDEX automation_runs_active ON automation_runs(status, not_before) WHERE status IN ('queued', 'running', 'waiting');
CREATE INDEX automation_runs_finished ON automation_runs(finished_at);

-- Steps of a run. A step inside a repeat group has one row per iteration.
CREATE TABLE automation_run_steps (
    id          INTEGER PRIMARY KEY,
    run_id      INTEGER NOT NULL REFERENCES automation_runs(id) ON DELETE CASCADE,
    step_id     TEXT    NOT NULL,
    iteration   INTEGER NOT NULL DEFAULT 1,
    position    INTEGER NOT NULL DEFAULT 0,
    type        TEXT    NOT NULL,
    status      TEXT    NOT NULL,                -- running, waiting, succeeded, failed, skipped, cancelled
    attempts    INTEGER NOT NULL DEFAULT 0,
    long        INTEGER NOT NULL DEFAULT 0,
    inputs      TEXT    NOT NULL DEFAULT '{}',   -- after templating; secrets hidden
    outputs     TEXT    NOT NULL DEFAULT '{}',
    log         TEXT    NOT NULL DEFAULT '',
    error       TEXT    NOT NULL DEFAULT '',
    started_at  INTEGER,
    finished_at INTEGER,
    UNIQUE (run_id, step_id, iteration)
);
CREATE INDEX automation_run_steps_long ON automation_run_steps(started_at) WHERE long = 1;

-- What a run (or a watching automation) waits for: an event of a kind with a
-- key (a pull request, job, request or environment), or just time. The
-- dispatcher matches arriving events; the worker times waits out.
CREATE TABLE automation_waits (
    id            INTEGER PRIMARY KEY,
    automation_id INTEGER NOT NULL REFERENCES automations(id) ON DELETE CASCADE,
    run_id        INTEGER REFERENCES automation_runs(id) ON DELETE CASCADE, -- NULL: a watch that starts a run when it times out
    step_id       TEXT    NOT NULL DEFAULT '',
    iteration     INTEGER NOT NULL DEFAULT 1,
    seq           INTEGER NOT NULL DEFAULT 1,
    project_id    INTEGER,                       -- NULL: events of any project
    kind          TEXT    NOT NULL DEFAULT '',   -- event kind; '' waits only for time
    key           TEXT    NOT NULL DEFAULT '',   -- '' matches any event of the kind
    match         TEXT    NOT NULL DEFAULT '',   -- extra conditions: state=passed,…
    timeout_at    INTEGER NOT NULL,
    created_at    INTEGER NOT NULL,
    resolved_at   INTEGER,
    outcome       TEXT    NOT NULL DEFAULT '',   -- event, timeout, cancelled
    event         TEXT    NOT NULL DEFAULT '{}'
);
CREATE UNIQUE INDEX automation_waits_step ON automation_waits(run_id, step_id, iteration, seq) WHERE run_id IS NOT NULL;
CREATE INDEX automation_waits_match ON automation_waits(kind, key) WHERE resolved_at IS NULL AND kind != '';
CREATE INDEX automation_waits_timeout ON automation_waits(timeout_at) WHERE resolved_at IS NULL;

-- Small per-automation memory: whether a count is above its threshold, …
CREATE TABLE automation_state (
    automation_id INTEGER NOT NULL REFERENCES automations(id) ON DELETE CASCADE,
    key           TEXT    NOT NULL,
    value         TEXT    NOT NULL,
    updated_at    INTEGER NOT NULL,
    PRIMARY KEY (automation_id, key)
);

-- Whether each system resource is low, and when it last fired, so a low disk
-- fires on crossing and at most hourly.
CREATE TABLE system_resource_state (
    resource      TEXT PRIMARY KEY,
    low           INTEGER NOT NULL DEFAULT 0,
    last_fired_at INTEGER NOT NULL DEFAULT 0
);
