-- Audits through OpenLLMs' agent tasks (WP-617, WP-ADR84).

-- The model that wrote the code a review round's audit reads (the asking
-- session's), so a gateway audit can leave that model's family out. Empty
-- for an audit a person asked for.
ALTER TABLE code_audits ADD COLUMN author_model TEXT NOT NULL DEFAULT '';

-- The OpenLLMs tasks audits ran as, and what Waypoint last told OpenLLMs of
-- how each answer's findings fared: the gateway ranks its audit models by
-- these reports (POST /v1/feedback by task id).
CREATE TABLE gateway_tasks (
    task_id         TEXT    PRIMARY KEY,        -- the gateway's id for the task
    audit_id        INTEGER NOT NULL REFERENCES code_audits(id) ON DELETE CASCADE,
    job_id          INTEGER NOT NULL,           -- the runner job that sent it
    model           TEXT    NOT NULL DEFAULT '', -- as asked: audit, free
    served_by       TEXT    NOT NULL DEFAULT '', -- provider/model that ran it to its end
    answered        INTEGER NOT NULL DEFAULT 0,  -- 1: its answer is the audit's findings
    created_at      INTEGER NOT NULL,
    verified        INTEGER NOT NULL DEFAULT 0,  -- as last reported
    false_positives INTEGER NOT NULL DEFAULT 0,
    reported_at     INTEGER,                     -- NULL: never reported
    tries           INTEGER NOT NULL DEFAULT 0,  -- failed reports since the last good one
    next_try_at     INTEGER NOT NULL DEFAULT 0,  -- not before (ms) after a failure
    error           TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX gateway_tasks_job ON gateway_tasks(job_id);
CREATE INDEX gateway_tasks_audit ON gateway_tasks(audit_id);
