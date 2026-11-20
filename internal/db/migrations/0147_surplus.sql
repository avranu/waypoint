-- OpenLLMs' surplus priority (WP-906, WP-ADR89): optional AI work sent only
-- on capacity that would otherwise go unused, and the prep results Waypoint
-- reports to OpenLLMs, which rates the models (its ADR 0051, 0067).

-- The priority each AI request was sent at: '' for the gateway's default,
-- 'surplus' for spare-capacity work. A surplus request the gateway turned
-- away is outcome 'refused': no model answered, nothing went wrong.
ALTER TABLE ai_calls ADD COLUMN priority TEXT NOT NULL DEFAULT '';

-- The priority a prep was made at: normal for the front of a queue,
-- surplus for the tasks behind it (prep ahead).
ALTER TABLE task_preps ADD COLUMN priority TEXT NOT NULL DEFAULT 'normal';
-- The X-Request-ID of the gateway request that answered ('' for another
-- provider), for the report on how its picks fared.
ALTER TABLE task_preps ADD COLUMN request_id TEXT NOT NULL DEFAULT '';

-- Each surplus prep made ahead of the front, and what came of it: whether
-- an agent got it while it was still current (it spared a normal prep).
-- How deep ahead to prep follows from these, by position.
CREATE TABLE prep_ahead (
    id         INTEGER PRIMARY KEY,
    issue_id   INTEGER NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
    position   INTEGER NOT NULL,           -- places behind the front preps when asked (1 = the first past them)
    outcome    TEXT    NOT NULL DEFAULT '', -- '' not known yet; used, redone, unused; refused, failed (no answer: not counted)
    created_at INTEGER NOT NULL,
    decided_at INTEGER
);
CREATE INDEX prep_ahead_open ON prep_ahead(issue_id) WHERE outcome = '';
CREATE INDEX prep_ahead_decided ON prep_ahead(decided_at);

-- A prep's picks by another model, made at surplus from the same bundle so
-- that model gets measured: never shown to anyone working the task.
CREATE TABLE prep_trials (
    id         INTEGER PRIMARY KEY,
    issue_id   INTEGER NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
    model      TEXT    NOT NULL,            -- the model that answered, as the gateway named it
    notes      TEXT    NOT NULL,            -- its picks, checked as a prep's are (JSON)
    request_id TEXT    NOT NULL DEFAULT '', -- the gateway request that answered
    error      TEXT    NOT NULL DEFAULT '', -- '' kept; else why no picks were kept (it failed, or a model left out answered): counts as a try, never scored
    created_at INTEGER NOT NULL
);
CREATE INDEX prep_trials_issue ON prep_trials(issue_id);

-- How a prep's file picks matched the files the task's merged pull requests
-- changed (tests and docs left out), by the model that answered, and the
-- report on its request to OpenLLMs: verified = hits, false positives =
-- picks the work didn't change.
CREATE TABLE prep_scores (
    id          INTEGER PRIMARY KEY,
    issue_id    INTEGER NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
    source      TEXT    NOT NULL,             -- prep (what agents get), trial
    priority    TEXT    NOT NULL,             -- normal, surplus
    model       TEXT    NOT NULL,
    picked      INTEGER NOT NULL,             -- files picked (files and symbols' files)
    truth       INTEGER NOT NULL,             -- files the work changed; 0 not counted
    hits        INTEGER NOT NULL,
    scored_at   INTEGER NOT NULL,
    request_id  TEXT    NOT NULL DEFAULT '',  -- '' when there is nothing to report on
    asked_at    INTEGER NOT NULL DEFAULT 0,   -- when the request was made
    reported_at INTEGER,                      -- the gateway took the report
    tries       INTEGER NOT NULL DEFAULT 0,
    next_try_at INTEGER NOT NULL DEFAULT 0,
    error       TEXT    NOT NULL DEFAULT '',
    UNIQUE(issue_id, source, model)
);
CREATE INDEX prep_scores_at ON prep_scores(scored_at);
CREATE INDEX prep_scores_report ON prep_scores(next_try_at) WHERE request_id != '' AND reported_at IS NULL;
