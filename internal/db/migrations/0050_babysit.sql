-- Babysitting (WP-134, ADR2 §2): Waypoint watches a pull request until it
-- merges, and gets each round of review comments and CI failures dealt with
-- by a fresh agent session on its owner's runner. One active per pull
-- request.
CREATE TABLE babysits (
    id            INTEGER PRIMARY KEY,
    pr_id         INTEGER NOT NULL REFERENCES pull_requests(id) ON DELETE CASCADE,
    -- watching, working, waiting_review, ready, needs_person; ended: merged, stopped, gave_up
    state         TEXT    NOT NULL DEFAULT 'watching',
    round         INTEGER NOT NULL DEFAULT 0,
    max_rounds    INTEGER NOT NULL DEFAULT 5,
    max_repeats   INTEGER NOT NULL DEFAULT 3,  -- the same check failing this many rounds in a row gives up
    model         TEXT    NOT NULL DEFAULT '', -- the rounds' Claude model; '' is the default (sonnet)
    owner_id      INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE, -- whose runner does the rounds
    started_by    INTEGER REFERENCES users(id) ON DELETE SET NULL,
    started_agent TEXT    NOT NULL DEFAULT '', -- the agent that started it, if one did
    started_via   TEXT    NOT NULL DEFAULT '', -- cli, ship, web, automation
    -- The agent session that started it: told when it ends or needs it.
    session_id    INTEGER REFERENCES agent_sessions(id) ON DELETE SET NULL,
    head_sha      TEXT    NOT NULL DEFAULT '',
    round_sha     TEXT    NOT NULL DEFAULT '', -- the head the last round was handed out for
    job_id        INTEGER REFERENCES runner_jobs(id) ON DELETE SET NULL, -- the round's agent job
    wait          TEXT    NOT NULL DEFAULT '', -- what it waits on, in words
    streaks       TEXT    NOT NULL DEFAULT '{}', -- failing check: rounds in a row it failed
    rounds        TEXT    NOT NULL DEFAULT '[]', -- each round: head, what it was given, what came of it
    told          TEXT    NOT NULL DEFAULT '', -- the last news people were told, so each is told once
    synced_at     INTEGER, -- review comments last read from the forge
    end_reason    TEXT    NOT NULL DEFAULT '',
    created_at    INTEGER NOT NULL,
    updated_at    INTEGER NOT NULL,
    ended_at      INTEGER
);
CREATE UNIQUE INDEX babysits_active ON babysits(pr_id) WHERE ended_at IS NULL;
CREATE INDEX babysits_pr ON babysits(pr_id, id);

-- Review comments on a pull request, from every reviewer (Waypoint's
-- findings, other bots, people), as the forge has them, and what was done
-- about each. Not the pull request author's own comments, nor Waypoint's
-- replies and status comments.
CREATE TABLE review_comments (
    id         INTEGER PRIMARY KEY,
    pr_id      INTEGER NOT NULL REFERENCES pull_requests(id) ON DELETE CASCADE,
    kind       TEXT    NOT NULL, -- inline (on the diff), review (a review's body), comment (the conversation), finding (one in Waypoint's review body)
    ext_id     TEXT    NOT NULL, -- the forge's id (a finding's: f<id>)
    thread_id  TEXT    NOT NULL DEFAULT '',
    finding_id INTEGER REFERENCES bot_findings(id) ON DELETE SET NULL, -- Waypoint's own finding
    author     TEXT    NOT NULL DEFAULT '',
    bot        INTEGER NOT NULL DEFAULT 0,
    path       TEXT    NOT NULL DEFAULT '',
    line       INTEGER,
    sha        TEXT    NOT NULL DEFAULT '', -- the head it was made on
    body       TEXT    NOT NULL DEFAULT '',
    url        TEXT    NOT NULL DEFAULT '',
    outdated   INTEGER NOT NULL DEFAULT 0, -- a later push changed the code it is on
    resolved   INTEGER NOT NULL DEFAULT 0, -- its thread is resolved on the forge
    -- new, seen, accepted, rejected, partial, needs_person, fixing, fixed, verified, dismissed
    verdict    TEXT    NOT NULL DEFAULT 'new',
    note       TEXT    NOT NULL DEFAULT '', -- why (rejected, partial, needs a person), as replied
    fix_sha    TEXT    NOT NULL DEFAULT '', -- the commit said to fix it
    reply_id   TEXT    NOT NULL DEFAULT '', -- Waypoint's reply on the forge
    reactions  TEXT    NOT NULL DEFAULT '[]', -- the emoji Waypoint put on it, in order
    created_at INTEGER NOT NULL, -- the forge's times
    updated_at INTEGER NOT NULL,
    seen_at    INTEGER NOT NULL, -- Waypoint first read it
    gone_at    INTEGER,          -- deleted on the forge
    UNIQUE (pr_id, kind, ext_id)
);
CREATE INDEX review_comments_finding ON review_comments(finding_id) WHERE finding_id IS NOT NULL;

-- Each verdict and reaction on a review comment, and who gave it: for review
-- scorecards (WP-100).
CREATE TABLE review_comment_events (
    id          INTEGER PRIMARY KEY,
    comment_id  INTEGER NOT NULL REFERENCES review_comments(id) ON DELETE CASCADE,
    pr_id       INTEGER NOT NULL REFERENCES pull_requests(id) ON DELETE CASCADE,
    old_verdict TEXT    NOT NULL DEFAULT '',
    verdict     TEXT    NOT NULL DEFAULT '',
    reaction    TEXT    NOT NULL DEFAULT '', -- the emoji put on the forge with it
    replied     INTEGER NOT NULL DEFAULT 0,
    by_kind     TEXT    NOT NULL, -- person, agent, babysitter, forge, verify, system
    by_name     TEXT    NOT NULL DEFAULT '',
    user_id     INTEGER REFERENCES users(id) ON DELETE SET NULL,
    round       INTEGER NOT NULL DEFAULT 0,
    sha         TEXT    NOT NULL DEFAULT '',
    note        TEXT    NOT NULL DEFAULT '',
    created_at  INTEGER NOT NULL
);
CREATE INDEX review_comment_events_comment ON review_comment_events(comment_id, id);
CREATE INDEX review_comment_events_pr ON review_comment_events(pr_id, id);
