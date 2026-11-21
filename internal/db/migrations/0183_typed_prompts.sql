-- The texts Waypoint has a runner type into a session (WP-1370): one row per
-- hand-over (a retry hands over again, so it records again). The hook that
-- reports a prompt is Waypoint's own iff the SHA-256 of the prompt's
-- normalised text (core.PromptSum) equals an unconsumed, unexpired row of
-- that session; the match consumes the row. No mark, heading or prefix is
-- ever enough.
CREATE TABLE typed_prompts (
    id          INTEGER PRIMARY KEY,
    session_id  INTEGER NOT NULL REFERENCES agent_sessions(id) ON DELETE CASCADE,
    hash        TEXT    NOT NULL,
    kind        TEXT    NOT NULL, -- review, compact, keep_warm, message, kickoff
    created_at  INTEGER NOT NULL,
    expires_at  INTEGER NOT NULL,
    consumed_at INTEGER
);
CREATE INDEX typed_prompts_lookup ON typed_prompts(session_id, hash);
CREATE INDEX typed_prompts_expiry ON typed_prompts(expires_at);

-- Who typed a prompt event: '' a person (or Claude Code's own report), else
-- the kind of the typed_prompts row its hook consumed. Later readers of the
-- timeline (who was in charge, what the person asked) read this, not the
-- text, which can no longer be matched once its row is consumed.
ALTER TABLE session_events ADD COLUMN typed_kind TEXT NOT NULL DEFAULT '';

-- Prompts recorded before this migration were told by their mark: keep them
-- as they were read (review prompts, and kickoffs autopilot's hook marked).
UPDATE session_events SET typed_kind = CASE WHEN detail LIKE 'Waypoint autopilot review of %' THEN 'review' ELSE 'kickoff' END
 WHERE kind = 'prompt' AND detail LIKE 'Waypoint autopilot%';
