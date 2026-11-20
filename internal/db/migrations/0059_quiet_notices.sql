-- Don't wake agents that have nothing left to do (WP-176).
-- A babysit tells its session when the pull request merges or ends cleanly
-- only if the agent asked (notify_end); a request says whether its answer
-- needs a follow-up: any answer, never, or only the listed choices or
-- decisions (followup, followup_on). A passive session message waits for the
-- agent's next prompt or start instead of continuing it at a stop.
ALTER TABLE babysits ADD COLUMN notify_end INTEGER NOT NULL DEFAULT 0;
ALTER TABLE requests ADD COLUMN followup TEXT NOT NULL DEFAULT 'any';
ALTER TABLE requests ADD COLUMN followup_on TEXT NOT NULL DEFAULT '[]';
ALTER TABLE session_messages ADD COLUMN passive INTEGER NOT NULL DEFAULT 0;
