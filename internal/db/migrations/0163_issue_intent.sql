-- A task's intent (WP-1196): a sentence or two saying what it's for, apart
-- from the description (the full write-up). A person writes or edits it on
-- the task's Summary tab; the description stays on Details. intent_agent is
-- the agent that wrote the current text ('' = a person), so an agent's
-- wording shows as a suggestion until a person keeps or rewrites it
-- (WP-780: agents don't own a task's intent).
ALTER TABLE issues ADD COLUMN intent TEXT NOT NULL DEFAULT '';
ALTER TABLE issues ADD COLUMN intent_agent TEXT NOT NULL DEFAULT '';
