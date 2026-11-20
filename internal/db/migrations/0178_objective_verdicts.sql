-- Objectives, verified (WP-927, WP-ADR142): a round's verdict is
-- no longer what the agent that worked the round reported. When a round's
-- work ends, Waypoint itself measures it (a job on the confirming person's
-- runner, in fresh checkouts) or has another model judge it (an audit), and
-- that reading alone decides whether the task closes or opens for the next
-- round. What wp objective check reports is kept as a working reading.

-- The repository the check runs in and the judge reads (owner/name; '': the
-- objective has neither a check nor a judge).
ALTER TABLE objectives ADD COLUMN repo TEXT NOT NULL DEFAULT '';
-- Commands that must keep passing (JSON array of shell): a round that
-- breaks one hasn't met the objective, whatever its check reads.
ALTER TABLE objectives ADD COLUMN keep_passing TEXT NOT NULL DEFAULT '[]';
-- A judged objective: what "done" means, in words, read by a model that
-- didn't do the work; the lowest severity of finding that keeps the task
-- open (critical, high, medium, low); the models asked, in order (JSON
-- array, []: the audit default).
ALTER TABLE objectives ADD COLUMN judge TEXT NOT NULL DEFAULT '';
ALTER TABLE objectives ADD COLUMN judge_bar TEXT NOT NULL DEFAULT '';
ALTER TABLE objectives ADD COLUMN judge_models TEXT NOT NULL DEFAULT '[]';
-- Where an objective in effect stands: '' a round is worked or waited for;
-- baseline: its starting point is being measured, before the first round;
-- verdict: a round's work ended and its reading is being taken;
-- blocked: it needs a person (phase_note says why, phase_why is the reason's
-- key). The task isn't handed out in any phase but ''.
ALTER TABLE objectives ADD COLUMN phase TEXT NOT NULL DEFAULT '';
ALTER TABLE objectives ADD COLUMN phase_at INTEGER;
ALTER TABLE objectives ADD COLUMN phase_why TEXT NOT NULL DEFAULT '';
ALTER TABLE objectives ADD COLUMN phase_note TEXT NOT NULL DEFAULT '';
-- blocked_from: the phase it was in when it blocked (a retry goes back there).
ALTER TABLE objectives ADD COLUMN blocked_from TEXT NOT NULL DEFAULT '';
-- The status the close that ended the round asked for: where the task goes
-- when the objective is met.
ALTER TABLE objectives ADD COLUMN close_status_id INTEGER REFERENCES statuses(id) ON DELETE SET NULL;
-- The reading under way: tries so far, the runner job or audit taking it,
-- and when the next try may start.
ALTER TABLE objectives ADD COLUMN verify_try INTEGER NOT NULL DEFAULT 0;
ALTER TABLE objectives ADD COLUMN verify_job INTEGER;
ALTER TABLE objectives ADD COLUMN verify_audit INTEGER;
ALTER TABLE objectives ADD COLUMN verify_at INTEGER;
-- A reading has two stages: the commands (a runner job), then the judge (an
-- audit) when there is one and the commands leave it to decide.
-- verify_reading: NULL until the commands have run, then the reading they
-- gave (0: the objective has no check, so there is none); verify_head: the
-- commit they ran at, which is the commit the judge reads.
ALTER TABLE objectives ADD COLUMN verify_reading INTEGER;
ALTER TABLE objectives ADD COLUMN verify_head TEXT NOT NULL DEFAULT '';
-- The reading that waits for a person to accept (a blocked verdict's).
ALTER TABLE objectives ADD COLUMN pending_measurement INTEGER;
-- The branch read ('': the repository's default), the commit the round
-- under way started from (the last one Waypoint read) and what it read
-- there.
ALTER TABLE objectives ADD COLUMN branch TEXT NOT NULL DEFAULT '';
ALTER TABLE objectives ADD COLUMN base_sha TEXT NOT NULL DEFAULT '';
ALTER TABLE objectives ADD COLUMN base_ok INTEGER;
ALTER TABLE objectives ADD COLUMN base_value REAL;
-- Why it ended, in words ("met: 0.8s", "closed as Done by Robin").
ALTER TABLE objectives ADD COLUMN ended_why TEXT NOT NULL DEFAULT '';
-- The watchdog (objective_health.go): since when nothing can move it on,
-- why, and when its person was last told.
ALTER TABLE objectives ADD COLUMN stuck_since INTEGER;
ALTER TABLE objectives ADD COLUMN stuck_why TEXT NOT NULL DEFAULT '';
ALTER TABLE objectives ADD COLUMN stuck_told_at INTEGER;
-- Rounds in a row it was told about (so it isn't told again every round).
ALTER TABLE objectives ADD COLUMN stalled_told INTEGER NOT NULL DEFAULT 0;
CREATE INDEX objectives_phase ON objectives(state, phase);

-- Who took a reading: agent (wp objective check, the worker's own: it never
-- decides a round), runner (Waypoint's job), judge (an audit), baseline
-- (Waypoint's reading before the first round).
ALTER TABLE objective_measurements ADD COLUMN source TEXT NOT NULL DEFAULT 'agent';
-- The objective's rev it was taken under: a reading of another command or
-- target never decides.
ALTER TABLE objective_measurements ADD COLUMN rev TEXT NOT NULL DEFAULT '';
ALTER TABLE objective_measurements ADD COLUMN job_id INTEGER;
ALTER TABLE objective_measurements ADD COLUMN audit_id INTEGER;
-- Files the round changed that the check's own commands name (JSON array):
-- a reading a person looks at before it counts.
ALTER TABLE objective_measurements ADD COLUMN touched TEXT NOT NULL DEFAULT '[]';
-- The commands that must keep passing and no longer did (JSON array).
ALTER TABLE objective_measurements ADD COLUMN broken TEXT NOT NULL DEFAULT '[]';
-- What the command last printed, when it gave no reading; a judge's summary.
ALTER TABLE objective_measurements ADD COLUMN note TEXT NOT NULL DEFAULT '';
-- A value's spread over its runs was nothing: it is compared exactly.
ALTER TABLE objective_measurements ADD COLUMN exact INTEGER NOT NULL DEFAULT 0;

-- A round's outcome is now "reading" from the end of its work until
-- Waypoint has read it, "worked" when work landed and the reading can't say
-- how much nearer it is, and "empty" when nothing landed (the round is
-- still owed).
ALTER TABLE objective_rounds ADD COLUMN base_sha TEXT NOT NULL DEFAULT '';
ALTER TABLE objective_rounds ADD COLUMN head_sha TEXT NOT NULL DEFAULT '';
ALTER TABLE objective_rounds ADD COLUMN note TEXT NOT NULL DEFAULT '';

-- Objectives already in effect with a check start from a reading of
-- Waypoint's own, like new ones.
UPDATE objectives SET phase = 'baseline', phase_at = CAST(strftime('%s', 'now') AS INTEGER) * 1000
WHERE state = 'active' AND check_cmd != '';

-- The merged pull requests an objective has taken account of, each once: a
-- merge is read by Waypoint itself, and a rule that closes the task on the
-- same merge then ends nothing more.
CREATE TABLE objective_merges (
    issue_id INTEGER NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
    url      TEXT    NOT NULL,
    round_no INTEGER NOT NULL,
    at       INTEGER NOT NULL,
    PRIMARY KEY (issue_id, url)
);
