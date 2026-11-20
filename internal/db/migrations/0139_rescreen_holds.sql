-- WP-837: the screen now reads some held tasks differently. A chain of two
-- agents' tasks is no longer held by rule (lineageHold 3), the install and web
-- flags no longer hold by themselves (the guard refuses them at run time), and
-- the model is asked with tighter wording. Each such hold is forgotten, so the
-- task is screened afresh:
--   * To Do (not a draft, not claimed): the minute sweep takes it, in batches.
--   * Any other status, drafts and closed tasks included: when it next changes,
--     is published or is reopened. Until then agents wait on it as unscreened,
--     the same as they waited on the hold. Left in place, the hold would
--     outlive the change: re-screening the same text keeps its verdict
--     (runScreen), so the task would come back to To Do still held.
-- The holds forgotten are those with no rule hit, which the model alone made
-- (rules is 'null' then, as Go writes a nil list, or '[]'), and those with a
-- lineage rule among their hits; a lineage hold of depth 3 or more, or one
-- that also hit a command rule, is held again by rule with no model asked.
-- Holds by command rules alone are unchanged, and so is every clear verdict.
-- The row is deleted rather than set pending, so a task no sweep reaches
-- doesn't sit in the status page's count of pending screens.
DELETE FROM task_safety
WHERE verdict = 'hold'
  AND json_valid(rules)
  AND (json_array_length(rules) = 0
       OR EXISTS (SELECT 1 FROM json_each(task_safety.rules)
                  WHERE value LIKE 'an agent wrote it while working on a task another agent wrote%'));
