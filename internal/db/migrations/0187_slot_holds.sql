-- Test slot holds (WP-1646): wp testsum and wp slot run take a per-machine
-- slot, and every completed hold (and every wait that gave up) is sent here,
-- so the slot policy (WP-ADR150: which runs are long, which slots they may
-- take) can be checked against what happened and simulated with other
-- settings. A machine posts its holds in batches under an id it made, so a
-- resend stores nothing twice.
--
-- Nothing prunes slot_holds: simulations need the history (WP-1054). A
-- machine is estimated to add about a hundred rows a day; that rate has not
-- been measured (WP-1655 measures it).
CREATE TABLE slot_holds (
    id             INTEGER PRIMARY KEY,
    user_id        INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    machine        TEXT    NOT NULL,
    hold_id        TEXT    NOT NULL,           -- made by the client
    name           TEXT    NOT NULL,           -- the slot name ("tests")
    repo           TEXT    NOT NULL DEFAULT '', -- git common dir
    tree           TEXT    NOT NULL DEFAULT '', -- worktree top level
    cmd            TEXT    NOT NULL DEFAULT '', -- the command key
    class          TEXT    NOT NULL,           -- short | long
    median_secs    REAL,                       -- of the history runs the class came from; NULL: none
    threshold_secs INTEGER NOT NULL DEFAULT 0,
    runs           INTEGER NOT NULL DEFAULT 0, -- history runs used (0..5)
    policy         TEXT    NOT NULL DEFAULT '',
    slots          INTEGER NOT NULL DEFAULT 0,
    long_slots     INTEGER NOT NULL DEFAULT 0,
    slot           INTEGER,                    -- the slot taken; NULL: gave up
    queued_at      INTEGER NOT NULL,           -- ms; when the wait began
    wait_ms        INTEGER NOT NULL DEFAULT 0,
    tree_wait_ms   INTEGER NOT NULL DEFAULT 0, -- the part spent behind the worktree's own long run
    hold_ms        INTEGER NOT NULL DEFAULT 0,
    gave_up        INTEGER NOT NULL DEFAULT 0,
    exit_code      INTEGER,                    -- NULL: not known
    holders        TEXT    NOT NULL DEFAULT '[]', -- JSON, at queued_at: [{slot,pid,since}]
    waiters        TEXT    NOT NULL DEFAULT '[]', -- JSON, at queued_at: [{pid,since,class,behind_own}]
    received_at    INTEGER NOT NULL,
    UNIQUE (user_id, machine, hold_id)
);
CREATE INDEX slot_holds_queued ON slot_holds(queued_at);
CREATE INDEX slot_holds_machine_queued ON slot_holds(user_id, machine, queued_at);

-- What each machine says of its own outbox, kept even when a batch is
-- refused, so a machine that cannot send shows.
CREATE TABLE slot_machines (
    user_id        INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    machine        TEXT    NOT NULL,
    pending        INTEGER NOT NULL DEFAULT 0, -- holds still waiting to be sent, as the machine last said
    pending_since  INTEGER,                    -- ms; queued_at of the oldest hold still waiting, as the machine last said; NULL when 0
    dropped        INTEGER NOT NULL DEFAULT 0, -- the machine's total of holds dropped from a full outbox
    rejected       INTEGER NOT NULL DEFAULT 0, -- the server's total of holds it refused as malformed
    last_report_at INTEGER NOT NULL,
    last_stored_at INTEGER,                    -- ms; the last time a hold was stored
    PRIMARY KEY (user_id, machine)
);
