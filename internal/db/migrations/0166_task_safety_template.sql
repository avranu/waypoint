-- WP-1266: a task Waypoint files from its own template (a protected-file
-- handoff) is Waypoint's text, not an agent's, yet it is stored as written
-- by "waypoint" and the safety screen read it as an agent's: it held each
-- handoff for a person, because its text asks a session to edit protected
-- files, which is the handoff's whole point (WP-D14). PR #398 waited
-- four days over four such holds. template_hash is the hash of the title and
-- description as Waypoint filed them; while the text still hashes the same,
-- the screen reads only the agents' comments on it. Only Waypoint's own code
-- writes it, so no agent can claim the trust by naming itself "waypoint".
ALTER TABLE task_safety ADD COLUMN template_hash TEXT NOT NULL DEFAULT '';
