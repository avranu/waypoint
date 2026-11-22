-- The texts Waypoint has a runner type into a session (WP-1370): one row per
-- hand-over (a retry hands over again, so it records again). The hook that
-- reports a prompt is Waypoint's own iff the SHA-256 of the prompt's
-- normalised text (core.PromptSum) equals an unconsumed, unexpired row of
-- that session; the match consumes the row. No mark, heading or prefix is
-- ever enough.
CREATE TABLE typed_prompts (
    id          INTEGER PRIMARY KEY,
    session_id  INTEGER REFERENCES agent_sessions(id) ON DELETE CASCADE, -- NULL: for a session not started yet; its first prompt binds it
    user_id     INTEGER NOT NULL DEFAULT 0, -- whose session may bind a NULL session_id
    hash        TEXT    NOT NULL,
    kind        TEXT    NOT NULL, -- review, compact, keep_warm, message, kickoff, start, exit
    created_at  INTEGER NOT NULL,
    expires_at  INTEGER NOT NULL,
    consumed_at INTEGER,
    consumed_key TEXT NOT NULL DEFAULT '' -- the hook invocation (HookEvent.Key) that consumed it
);
CREATE INDEX typed_prompts_lookup ON typed_prompts(session_id, hash);
CREATE INDEX typed_prompts_unbound ON typed_prompts(user_id, hash) WHERE session_id IS NULL;
CREATE INDEX typed_prompts_expiry ON typed_prompts(expires_at);

-- Who typed a prompt event: '' a person (or Claude Code's own report), else
-- the kind of the typed_prompts row its hook consumed. Later readers of the
-- timeline (who was in charge, what the person asked) read this, not the
-- text, which can no longer be matched once its row is consumed.
ALTER TABLE session_events ADD COLUMN typed_kind TEXT NOT NULL DEFAULT '';

-- Prompts recorded before this migration were told by their mark, which the
-- code before it read case-sensitively (strings.HasPrefix): keep them as they
-- were read (review prompts, and kickoffs autopilot's hook marked). LIKE is
-- case-insensitive in SQLite, so the test is an exact, binary prefix compare.
UPDATE session_events SET typed_kind = CASE WHEN substr(detail, 1, 29) = 'Waypoint autopilot review of ' THEN 'review' ELSE 'kickoff' END
 WHERE kind = 'prompt' AND substr(detail, 1, 18) = 'Waypoint autopilot';
