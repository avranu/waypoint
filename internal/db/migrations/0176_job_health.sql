-- Whether each scheduled job works, kept across restarts (WP-999, WP-ADR139).
-- Before this a job's start and end lived in memory, and a job that warned on
-- every run looked the same as one that worked.

-- One row a scheduled job: its last run and whether it was clean.
CREATE TABLE job_health (
    name          TEXT PRIMARY KEY,
    -- how often it runs; 0 for a job that runs once when the server starts
    period_ms     INTEGER NOT NULL DEFAULT 0,
    runs          INTEGER NOT NULL DEFAULT 0,
    skips         INTEGER NOT NULL DEFAULT 0,
    last_start    INTEGER,
    last_end      INTEGER,
    last_ms       INTEGER NOT NULL DEFAULT 0,
    max_ms        INTEGER NOT NULL DEFAULT 0,
    -- clean, warned, failed, skipped, stopped, crashed or interrupted
    last_result   TEXT NOT NULL DEFAULT '',
    last_note     TEXT NOT NULL DEFAULT '',
    last_clean_at INTEGER,
    -- when the admins were told it is stuck or silent; cleared by its next run
    alerted_at    INTEGER,
    alerted_for   TEXT NOT NULL DEFAULT '',
    -- when the oldest run going at the last save began; NULL when none. If the
    -- server dies, this is what it was doing.
    running_since INTEGER,
    -- when the job began to skip every run because the last was still going;
    -- NULL once a run ends any other way. A busy flag dies with the process,
    -- so it is cleared at the server's start.
    busy_since    INTEGER
);

-- One row for each thing that goes wrong: a warning or an error, by where it
-- was logged (a job's name, or requests, events, background), its message and
-- what it was about (a repo, a project, a forge…). "repo sync failed" for
-- seven repos is seven rows, so an eighth is new.
CREATE TABLE job_problems (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    source       TEXT NOT NULL,
    message      TEXT NOT NULL,
    target       TEXT NOT NULL DEFAULT '',
    level        TEXT NOT NULL,
    first_at     INTEGER NOT NULL,
    last_at      INTEGER NOT NULL,
    count        INTEGER NOT NULL,
    -- the current run of occurrences with no quiet gap between them
    streak_since INTEGER NOT NULL,
    streak_count INTEGER NOT NULL,
    -- the latest record's attributes, secrets blanked
    detail       TEXT NOT NULL DEFAULT '',
    alerted_at   INTEGER,
    cleared_at   INTEGER,
    -- how long the server was not running since this was last seen: quiet
    -- while it was down says nothing about the problem being over
    away_ms      INTEGER NOT NULL DEFAULT 0,
    -- the same, summed over the current streak: downtime inside it is not
    -- time the problem lasted
    streak_away_ms INTEGER NOT NULL DEFAULT 0,
    acked_at     INTEGER,
    acked_by     INTEGER REFERENCES users(id) ON DELETE SET NULL,
    ack_reason   TEXT NOT NULL DEFAULT '',
    UNIQUE (source, message, target)
);
CREATE INDEX job_problems_open ON job_problems(last_at) WHERE cleared_at IS NULL;

-- What each source did in each hour, kept 90 days: the trend behind "is this
-- getting worse".
CREATE TABLE job_hours (
    source   TEXT NOT NULL,
    hour     INTEGER NOT NULL, -- the hour's first millisecond
    runs     INTEGER NOT NULL DEFAULT 0,
    unclean  INTEGER NOT NULL DEFAULT 0,
    skips    INTEGER NOT NULL DEFAULT 0,
    warnings INTEGER NOT NULL DEFAULT 0,
    errors   INTEGER NOT NULL DEFAULT 0,
    noted    INTEGER NOT NULL DEFAULT 0,
    ms_total INTEGER NOT NULL DEFAULT 0,
    ms_max   INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (source, hour)
) WITHOUT ROWID;

-- One row for each time the server started: when it last checked its jobs,
-- when it was last seen alive (the watchdog's ticker, whether or not the
-- check finishes), and when it shut down, if it did. A row with no
-- stopped_at that is not the latest is a process that was killed (OOM,
-- SIGKILL, a panic).
CREATE TABLE server_runs (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    started_at INTEGER NOT NULL,
    checked_at INTEGER NOT NULL,
    alive_at   INTEGER NOT NULL,
    stopped_at INTEGER,
    version    TEXT NOT NULL DEFAULT ''
);
