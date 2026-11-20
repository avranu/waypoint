-- Agentic prep trials (WP-250, WP-ADR104): at surplus, an OpenLLMs agent
-- task reads the task's repository with tools and names the files, symbols
-- and tests it needs, so its picks can be compared with the chat prep's on
-- the same tasks. Its gateway task has no audit behind it, so a gateway
-- task now belongs to an audit or to a task (issue). Nothing references
-- gateway_tasks, so foreign keys stay on while it is rebuilt. The rebuilt
-- table carries every column the table had (0140, 0156_session_commands);
-- TestMigrationsKeepAddedColumns fails if a rebuild leaves one behind.
CREATE TABLE gateway_tasks_new (
    task_id         TEXT    PRIMARY KEY,        -- the gateway's id for the task
    audit_id        INTEGER REFERENCES code_audits(id) ON DELETE CASCADE, -- NULL: a prep trial's
    issue_id        INTEGER REFERENCES issues(id) ON DELETE CASCADE,      -- a prep trial's task
    job_id          INTEGER NOT NULL,           -- the runner job that sent it
    model           TEXT    NOT NULL DEFAULT '', -- as asked: audit, free, prep
    served_by       TEXT    NOT NULL DEFAULT '', -- provider/model that ran it to its end
    answered        INTEGER NOT NULL DEFAULT 0,  -- 1: its answer is the audit's findings
    created_at      INTEGER NOT NULL,
    verified        INTEGER NOT NULL DEFAULT 0,  -- as last reported
    false_positives INTEGER NOT NULL DEFAULT 0,
    reported_at     INTEGER,                     -- NULL: never reported
    tries           INTEGER NOT NULL DEFAULT 0,  -- failed reports since the last good one
    next_try_at     INTEGER NOT NULL DEFAULT 0,  -- not before (ms) after a failure
    error           TEXT    NOT NULL DEFAULT '',
    status          TEXT    NOT NULL DEFAULT '', -- queued, running, succeeded, … ('': not heard yet)
    input_tokens    INTEGER NOT NULL DEFAULT 0,
    output_tokens   INTEGER NOT NULL DEFAULT 0,
    polled_at       INTEGER NOT NULL DEFAULT 0,  -- its runner last asked how it goes (ms)
    grew_at         INTEGER NOT NULL DEFAULT 0,  -- its token count last grew (ms)
    CHECK ((audit_id IS NULL) != (issue_id IS NULL))
);
INSERT INTO gateway_tasks_new (task_id, audit_id, job_id, model, served_by, answered, created_at, verified, false_positives, reported_at, tries, next_try_at, error, status, input_tokens, output_tokens, polled_at, grew_at)
SELECT task_id, audit_id, job_id, model, served_by, answered, created_at, verified, false_positives, reported_at, tries, next_try_at, error, status, input_tokens, output_tokens, polled_at, grew_at
FROM gateway_tasks;
DROP TABLE gateway_tasks;
ALTER TABLE gateway_tasks_new RENAME TO gateway_tasks;
CREATE INDEX gateway_tasks_job ON gateway_tasks(job_id);
CREATE INDEX gateway_tasks_audit ON gateway_tasks(audit_id);

-- A trial is a chat model rereading the prep's bundle (WP-906), or an agent
-- exploring the repository itself (WP-250). Chat trials count toward a
-- task's chat trials only; an agent trial is one per text of the task
-- (text_hash: the prep's when the trial's job was queued; '' for a chat
-- trial, and for an agent's whose task was prepared again while it ran)
-- and one per runner job (job_id; 0 for a chat trial and a try never queued).
ALTER TABLE prep_trials ADD COLUMN kind TEXT NOT NULL DEFAULT 'chat';
ALTER TABLE prep_trials ADD COLUMN text_hash TEXT NOT NULL DEFAULT '';
ALTER TABLE prep_trials ADD COLUMN job_id INTEGER NOT NULL DEFAULT 0;

-- What an agent taking a task is shown of its prep (WP-250, WP-ADR104):
-- nothing, the chat prep's picks, or the agentic prep's. Drawn once per
-- task, the first time an agent is given its ready prep, with each arm's
-- share the probability that it is the best one on what agents did with
-- the arms so far (prep_policy.go). The share the arm had is kept, so the
-- tasks can be weighed by it later, and so are the things known of the task
-- before the draw that the estimate adjusts for.
CREATE TABLE prep_assignments (
    issue_id    INTEGER PRIMARY KEY REFERENCES issues(id) ON DELETE CASCADE,
    arm         TEXT    NOT NULL,            -- none, chat, agent; a changed prompt or pipeline is a new arm
    policy      TEXT    NOT NULL,            -- thompson (drawn by share), best (the taker sends no traces: not a test), import (the prep experiment's)
    propensity  REAL    NOT NULL,            -- the share the arm had for this task
    shares      TEXT    NOT NULL DEFAULT '{}', -- every arm open to the task, and its share
    same        INTEGER NOT NULL DEFAULT 0,  -- 1: the chat and agentic picks were the same files, so it speaks for both
    text_len    INTEGER NOT NULL DEFAULT 0,  -- the task's title and description, in characters, then
    agent_ready INTEGER NOT NULL DEFAULT 0,  -- 1: agentic picks of the prep's text were there
    assigned_at INTEGER NOT NULL,
    exposed_at  INTEGER                      -- an agent taking the task was given it; NULL: not yet
);
CREATE INDEX prep_assignments_exposed ON prep_assignments(exposed_at) WHERE exposed_at IS NOT NULL;

-- The policy as it stood each time the shares moved: the history its
-- report shows, and nothing it reads back to decide.
CREATE TABLE prep_policy_snapshots (
    id     INTEGER PRIMARY KEY,
    at     INTEGER NOT NULL,
    shares TEXT    NOT NULL,  -- {arm: share} for a task with every arm open
    arms   TEXT    NOT NULL,  -- [{arm, n, effect, lo, hi, p_best, blocked}]
    reason TEXT    NOT NULL   -- what moved
);
CREATE INDEX prep_policy_snapshots_at ON prep_policy_snapshots(at);

-- The prep experiment (shown 4 : hidden 1, WP-254) ends here: the policy
-- takes over what it asked, with nothing as one of its arms. Every ready
-- prep with picks keeps what it was, at the share its run gave it, so the
-- tasks already measured count and an open task's agent gets what its prep
-- was drawn for.
INSERT INTO prep_assignments(issue_id, arm, policy, propensity, shares, text_len, assigned_at, exposed_at)
SELECT p.issue_id, CASE WHEN p.held_out THEN 'none' ELSE 'chat' END, 'import',
       coalesce((SELECT CASE WHEN p.held_out THEN 1.0 * json_extract(r.weights, '$.hidden') ELSE 1.0 * json_extract(r.weights, '$.shown') END
                        / nullif(coalesce(json_extract(r.weights, '$.hidden'), 0) + coalesce(json_extract(r.weights, '$.shown'), 0), 0)
                 FROM experiment_units u JOIN experiment_runs r ON r.id = u.run_id
                 WHERE r.experiment = 'prep' AND u.issue_id = p.issue_id ORDER BY u.assigned_at LIMIT 1),
                CASE WHEN p.held_out THEN 0.2 ELSE 0.8 END),
       '{}', length(i.title) + length(i.description), p.created_at, p.shown_at
FROM task_preps p JOIN issues i ON i.id = p.issue_id
WHERE p.state = 'ready' AND json_valid(p.notes)
  AND coalesce(json_array_length(p.notes, '$.files'), 0) + coalesce(json_array_length(p.notes, '$.symbols'), 0) > 0;

UPDATE experiment_runs SET stopped_at = unixepoch() * 1000,
       stop_reason = 'replaced by the prep policy (WP-250, WP-ADR104), which carries its tasks on'
WHERE experiment = 'prep' AND stopped_at IS NULL;
