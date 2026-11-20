-- An agent's design note written for a task a person filed counts on that
-- request (WP-189), but the person never saw it (WP-409, WP-ADR96): they are no
-- longer recorded as its approver, nor as the reviewer of the agent's
-- changes to it. The request (requested_issue_id, requested_by) stays the
-- note's provenance, and approved_at stays so it still counts.
UPDATE design_notes SET approved_by = NULL
    WHERE requested_issue_id IS NOT NULL AND approved_by = requested_by;

-- The agent's own revisions stamped "requested in …" on such a note, with
-- the requester as reviewer: no reviewer, and an addition is applied (took
-- effect without a person), not approved. A person's own review of an
-- agent's change (ReviewDesign) whose note happens to read "requested in …"
-- isn't this stamp unless it is on a requested note by the requester, and
-- its review_note is left as written either way.
UPDATE design_revisions SET reviewed_by = NULL, reviewed_at = NULL,
    state = CASE WHEN state = 'approved' THEN 'applied' ELSE state END
    WHERE agent <> '' AND state IN ('approved', 'applied')
      AND review_note GLOB 'requested in *'
      AND reviewed_by = (SELECT n.requested_by FROM design_notes n
          WHERE n.id = design_revisions.note_id AND n.requested_issue_id IS NOT NULL);
