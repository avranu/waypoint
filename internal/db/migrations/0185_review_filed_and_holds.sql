-- Filing a finding instead of fixing it ends a review round (WP-1547), and a
-- ship held for findings still to decide returned text and recorded nothing
-- (WP-1555). Both are measured now.

-- filed: how many of the round's findings were filed (confirmed with an
-- issue linked, medium or lower: findingFiled). They are among left_open.
ALTER TABLE review_again_decisions ADD COLUMN filed INTEGER NOT NULL DEFAULT 0;

-- filed: the finding was filed when its round was decided, so a signal or
-- a round that came from one can be told from one that came from a fix.
ALTER TABLE review_finding_scores ADD COLUMN filed INTEGER NOT NULL DEFAULT 0;

-- Decisions and scores from before this have their filed counted from the
-- findings as they stand now: a decision records it as it stood then, and
-- a mark changed since is the only difference. Left at 0, the review
-- health's 30 days would read every earlier filing as a fix. A decision's
-- findings are its audit's and its check's (review-round:N:check).
UPDATE review_again_decisions SET filed = (SELECT count(*) FROM code_audit_findings f
    WHERE (f.audit_id = review_again_decisions.audit_id
        OR f.audit_id IN (SELECT ca.id FROM code_audits ca WHERE ca.request_key = 'review-round:' || review_again_decisions.round_id || ':check'))
      AND f.state = 'confirmed' AND f.issue_id IS NOT NULL AND f.severity NOT IN ('critical', 'high'));

UPDATE review_finding_scores SET filed = EXISTS(SELECT 1 FROM code_audit_findings f WHERE f.id = review_finding_scores.finding_id
    AND f.state = 'confirmed' AND f.issue_id IS NOT NULL AND f.severity NOT IN ('critical', 'high'));

-- A wp ship refused for findings still to decide. check_name: which refusal
-- held it: audit (shipAudited: the round's audit read by the session),
-- round (reviewAgain: the round's own findings), marks (checkMarks: what the
-- check of the session's marks found). audit_id is the audit whose findings
-- were to decide; round_id the round the ship was for (0 when none).
CREATE TABLE review_ship_holds (
    id         INTEGER PRIMARY KEY,
    issue_id   INTEGER NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
    audit_id   INTEGER NOT NULL DEFAULT 0,
    round_id   INTEGER NOT NULL DEFAULT 0,
    check_name TEXT    NOT NULL,
    findings   INTEGER NOT NULL DEFAULT 0,
    created_at INTEGER NOT NULL
);
CREATE INDEX review_ship_holds_issue ON review_ship_holds(issue_id, created_at);
