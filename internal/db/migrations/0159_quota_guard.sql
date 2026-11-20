-- The daily quota guard (WP-932, WP-ADR112): once a day, how much
-- of the person's weekly Claude Code quota automation used, in which phase
-- of the week, and whether it used any while the limits should have held
-- it back. One row per check; report is the whole QuotaGuardCheck as JSON.
-- Kept 90 days.
CREATE TABLE quota_guard_checks (
    id      INTEGER PRIMARY KEY,
    user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    at      INTEGER NOT NULL, -- ms, when it ran
    from_at INTEGER NOT NULL, -- ms, the window it judged: [from_at, to_at)
    to_at   INTEGER NOT NULL,
    breach  INTEGER NOT NULL DEFAULT 0,
    report  TEXT    NOT NULL DEFAULT '{}'
);
CREATE INDEX quota_guard_checks_user ON quota_guard_checks(user_id, to_at);

-- The live quota guard (WP-932, WP-ADR112). What the person set (each field
-- null: the default from their own usage), what the minute sweep holds now
-- and the baselines the daily check measured, each session's spend as its
-- hooks last counted it, and every time the guard told or stopped a session.
CREATE TABLE quota_guard_settings (
    user_id    INTEGER PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    settings   TEXT    NOT NULL DEFAULT '{}', -- QuotaGuardSettings
    updated_at INTEGER NOT NULL,
    updated_by INTEGER REFERENCES users(id) ON DELETE SET NULL
);

CREATE TABLE quota_guard_state (
    user_id INTEGER PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    state   TEXT    NOT NULL DEFAULT '{}' -- guardWatch: hold, quiet, baselines, the over phase's start
);

-- One row per session per pulse (at most every 30 s per machine while it
-- works); cost is the session tree's list-price total so far. Kept 2 days.
CREATE TABLE session_pulses (
    id      INTEGER PRIMARY KEY,
    user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    machine TEXT    NOT NULL,
    cli_id  TEXT    NOT NULL,
    at      INTEGER NOT NULL,
    cost    REAL    NOT NULL,
    auto    INTEGER NOT NULL DEFAULT 0, -- automation drove it
    model   TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX session_pulses_user ON session_pulses(user_id, at);
CREATE INDEX session_pulses_session ON session_pulses(user_id, machine, cli_id, at);

CREATE TABLE quota_guard_trips (
    id         INTEGER PRIMARY KEY,
    user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    session_id INTEGER REFERENCES agent_sessions(id) ON DELETE SET NULL,
    machine    TEXT    NOT NULL DEFAULT '',
    cli_id     TEXT    NOT NULL DEFAULT '',
    rule       TEXT    NOT NULL,          -- churn, session, model, hours, idle, hold
    action     TEXT    NOT NULL,          -- told, stopped, held
    cost       REAL    NOT NULL DEFAULT 0,
    detail     TEXT    NOT NULL DEFAULT '',
    at         INTEGER NOT NULL
);
CREATE INDEX quota_guard_trips_user ON quota_guard_trips(user_id, at);

-- The minute sweep reads the last hour's automation pulses and finished
-- Claude jobs across people.
CREATE INDEX session_pulses_at ON session_pulses(at);
CREATE INDEX runner_jobs_claude_finished ON runner_jobs(finished_at) WHERE cli = 'claude';

-- When the person last turned the quota limits off, so the daily check
-- reads them off only from then to quota_off_until (0: not known).
ALTER TABLE autopilot_settings ADD COLUMN quota_off_at INTEGER NOT NULL DEFAULT 0;
