-- The head of the review prompt Waypoint typed for a step, whitespace
-- squeezed (WP-1416): the hook that reports a prompt recognises Waypoint's
-- own by this, whatever the harness wrapped round it, rather than by how it
-- starts.
ALTER TABLE autopilot_steps ADD COLUMN sent TEXT NOT NULL DEFAULT '';
