-- Objectives, a diagnose round (WP-1449, WP-ADR142): rounds that come no
-- nearer in a row don't ask the worker for another attempt at the same
-- thing. The next round is a diagnose round: it ships no code, and finds
-- out why, and the worker records what it found for the rounds after.

-- Rounds in a row Waypoint read and found no nearer (none, worked); not
-- stalled, which also counts rounds that landed nothing.
ALTER TABLE objectives ADD COLUMN no_better INTEGER NOT NULL DEFAULT 0;
-- How many such rounds in a row make the next one a diagnose round (0:
-- never). Objectives that exist already take the default, 2, on upgrade:
-- deliberate, so they get the diagnose round too (0 turns it off).
ALTER TABLE objectives ADD COLUMN diagnose_after INTEGER NOT NULL DEFAULT 2;
-- What the round under way is: '' worked as usual, diagnose.
ALTER TABLE objectives ADD COLUMN round_kind TEXT NOT NULL DEFAULT '';
-- The latest diagnosis (an objective_rounds row), read by every round after
-- it until one makes progress or the objective is set again.
ALTER TABLE objectives ADD COLUMN diagnosis_id INTEGER;

-- A round that was a diagnose round has outcome 'diagnosed': what the
-- worker (or a person) found, who wrote it.
ALTER TABLE objective_rounds ADD COLUMN diagnosis TEXT NOT NULL DEFAULT '';
ALTER TABLE objective_rounds ADD COLUMN diagnosis_by TEXT NOT NULL DEFAULT '';
