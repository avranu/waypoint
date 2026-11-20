-- CI hosts and what their jobs used (WP-231). A reporter on each runner host
-- posts its resources and, per job container, CPU time and peak memory
-- read from the container's cgroup. The CI board's pool comes from these,
-- and later the runners size their slots from the jobs' history.
CREATE TABLE ci_hosts (
    forge_id     INTEGER NOT NULL REFERENCES forges(id) ON DELETE CASCADE,
    name         TEXT    NOT NULL, -- the runner's name on the forge
    labels       TEXT    NOT NULL DEFAULT '[]',
    capacity     INTEGER NOT NULL DEFAULT 0,
    cpus         INTEGER NOT NULL DEFAULT 0,
    memory       INTEGER NOT NULL DEFAULT 0, -- bytes
    pool_cpus    REAL    NOT NULL DEFAULT 0, -- what all its jobs together may use
    pool_memory  INTEGER NOT NULL DEFAULT 0,
    job_cpus     REAL    NOT NULL DEFAULT 0, -- what one job may use
    job_memory   INTEGER NOT NULL DEFAULT 0,
    disk_total   INTEGER NOT NULL DEFAULT 0,
    disk_free    INTEGER NOT NULL DEFAULT 0,
    load1        REAL    NOT NULL DEFAULT 0,
    running      INTEGER NOT NULL DEFAULT 0, -- job containers running at the report
    version      TEXT    NOT NULL DEFAULT '',
    reported_at  INTEGER NOT NULL,
    PRIMARY KEY (forge_id, name)
);

-- One row per job or service container a host ran.
CREATE TABLE ci_job_usage (
    forge_id    INTEGER NOT NULL REFERENCES forges(id) ON DELETE CASCADE,
    host        TEXT    NOT NULL,
    container   TEXT    NOT NULL,
    task_id     INTEGER NOT NULL,
    job         TEXT    NOT NULL, -- the job's key, from the container's name
    role        TEXT    NOT NULL DEFAULT 'job', -- job, service
    -- filled from the forge's task when the board reads it
    repo        TEXT    NOT NULL DEFAULT '',
    workflow    TEXT    NOT NULL DEFAULT '',
    name        TEXT    NOT NULL DEFAULT '',
    status      TEXT    NOT NULL DEFAULT '',
    started_at  INTEGER NOT NULL,
    ended_at    INTEGER, -- NULL while it runs
    cpu_seconds REAL    NOT NULL DEFAULT 0,
    mem_peak    INTEGER NOT NULL DEFAULT 0,
    mem_limit   INTEGER NOT NULL DEFAULT 0,
    cpu_limit   REAL    NOT NULL DEFAULT 0,
    io_read     INTEGER NOT NULL DEFAULT 0,
    io_write    INTEGER NOT NULL DEFAULT 0,
    reported_at INTEGER NOT NULL,
    PRIMARY KEY (forge_id, host, container)
);
CREATE INDEX ci_job_usage_task ON ci_job_usage(forge_id, task_id);
CREATE INDEX ci_job_usage_job ON ci_job_usage(forge_id, repo, workflow, name, started_at);
CREATE INDEX ci_job_usage_started ON ci_job_usage(started_at);

-- Runner-fault re-runs of a babysat head: "sha n first-seen-ms last-ms".
ALTER TABLE babysits ADD COLUMN fault_reruns TEXT NOT NULL DEFAULT '';
