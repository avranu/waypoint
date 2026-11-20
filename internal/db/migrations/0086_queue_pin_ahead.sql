-- Queue pins keep what is ahead of them (WP-311). agent_order was a pin's
-- place in the queue (WP-87); when a pinned task above it finished, its
-- place went to the automatic order, so the next overdue task jumped
-- ahead of the tasks a person had put at the top. A pin is now the number
-- of automatically ordered tasks ahead of it (agent_ahead: 0 = before all
-- of them) and, among pins with the same number, agent_order (lowest
-- first). Every pin set so far was put at the top, one after another, so
-- each goes back to the top in its order.
ALTER TABLE issues ADD COLUMN agent_ahead INTEGER NOT NULL DEFAULT 0;
