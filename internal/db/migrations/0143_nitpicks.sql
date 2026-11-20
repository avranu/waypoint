-- Severity scores for review comments and review findings (WP-695,
-- WP-ADR88). The decision model reads only the text of a
-- comment or finding and a few facts about it, never the code.

-- What a babysat pull request's comment-only round would have been given,
-- decided once per set of comments: waived (every one scored a nitpick, so
-- no round and they don't hold the merge), holdout (they would have been
-- waived; they went to a round as before, for comparison), held (one scored
-- above a nitpick), unscored (the model gave no answer; they went to a
-- round as before).
CREATE TABLE nitpick_decisions (
    id         INTEGER PRIMARY KEY,
    pr_id      INTEGER NOT NULL REFERENCES pull_requests(id) ON DELETE CASCADE,
    babysit_id INTEGER REFERENCES babysits(id) ON DELETE SET NULL,
    head_sha   TEXT    NOT NULL DEFAULT '',
    arm        TEXT    NOT NULL,
    comments   TEXT    NOT NULL DEFAULT '[]', -- review_comments ids, sorted
    set_key    TEXT    NOT NULL DEFAULT '',   -- the comments and their texts: an edited one makes another set
    max_score  INTEGER NOT NULL DEFAULT 0,
    model      TEXT    NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL
);
CREATE INDEX nitpick_decisions_pr ON nitpick_decisions(pr_id, set_key);
CREATE INDEX nitpick_decisions_at ON nitpick_decisions(created_at);

-- Each comment's score, 1 (cosmetic) to 10 (critical), for the text it had
-- (body_hash): an edited comment is scored again. waived_at: a waived
-- decision let it through; it doesn't hold the merge.
CREATE TABLE review_comment_scores (
    comment_id  INTEGER PRIMARY KEY REFERENCES review_comments(id) ON DELETE CASCADE,
    pr_id       INTEGER NOT NULL REFERENCES pull_requests(id) ON DELETE CASCADE,
    score       INTEGER NOT NULL,
    probs       TEXT    NOT NULL DEFAULT '{}',
    model       TEXT    NOT NULL DEFAULT '',
    body_hash   TEXT    NOT NULL,
    decision_id INTEGER REFERENCES nitpick_decisions(id) ON DELETE SET NULL,
    waived_at   INTEGER,
    scored_at   INTEGER NOT NULL
);
CREATE INDEX review_comment_scores_pr ON review_comment_scores(pr_id);

-- A nitpick still open when its pull request merged goes into that pull
-- request's nitpick log (an audit, requested_agent 'review-nitpicks'); the
-- audit's finding links back to the comment.
ALTER TABLE review_comments ADD COLUMN audit_finding_id INTEGER REFERENCES code_audit_findings(id) ON DELETE SET NULL;
-- When the merged pull request's nitpick sweep finished; a merge with
-- waived nitpicks and none is swept on its next event.
ALTER TABLE pull_requests ADD COLUMN nitpicks_swept_at INTEGER;

-- Whether a task's adversarial review before its pull request gets a
-- second round, decided at the wp ship that would open the pull request
-- from the review's findings and what the session marked on them: again (a
-- second round was scheduled), enough (the pull request opened), holdout
-- (it would have had a second round; it opened, for comparison), unscored
-- (the model failed or gave no usable score; it opened; a model that
-- can't be asked now records nothing).
CREATE TABLE review_again_decisions (
    id            INTEGER PRIMARY KEY,
    round_id      INTEGER NOT NULL REFERENCES work_log(id) ON DELETE CASCADE,
    issue_id      INTEGER NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
    audit_id      INTEGER REFERENCES code_audits(id) ON DELETE SET NULL,
    findings      INTEGER NOT NULL DEFAULT 0,
    fixed         INTEGER NOT NULL DEFAULT 0,
    rejected      INTEGER NOT NULL DEFAULT 0,
    left_open     INTEGER NOT NULL DEFAULT 0,
    issues_score  INTEGER NOT NULL DEFAULT 0, -- 1-10: how serious the worst finding is
    fixes_score   INTEGER NOT NULL DEFAULT 0, -- 1-10: how incomplete or risky the fixes look
    probs         TEXT    NOT NULL DEFAULT '{}',
    model         TEXT    NOT NULL DEFAULT '',
    arm           TEXT    NOT NULL,
    next_round_id INTEGER REFERENCES work_log(id) ON DELETE SET NULL,
    created_at    INTEGER NOT NULL
);
CREATE UNIQUE INDEX review_again_round ON review_again_decisions(round_id);
CREATE INDEX review_again_at ON review_again_decisions(created_at);
