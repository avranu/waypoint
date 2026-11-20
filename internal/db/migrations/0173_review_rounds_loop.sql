-- Review rounds that loop while they keep finding what matters (WP-1320).
-- review_again_decisions was one decision per run (only its first round
-- could lead to another, WP-ADR88); now every round before the pull request
-- is decided at its wp ship, from each finding's score.

-- round_index: the round's place in its run's rounds before the pull
-- request (1 the first). Rows from before were all first rounds.
-- tier: the change's stakes (review_stakes.go) the bars were set by.
-- worst: the highest standing score among the round's findings not raised
-- by an earlier round of the run (0-10, a finding's severity times how far
-- it held up); worst_all the same over every finding, repeats included.
-- fixes_mean: the fixes question's expected score (1-9).
-- novel, repeats: the findings that were new, and those an earlier round
-- had raised already.
-- yield_p: the draw from the round index's record of further rounds
-- finding something (the low_yield arm), -1 when none was drawn.
ALTER TABLE review_again_decisions ADD COLUMN round_index INTEGER NOT NULL DEFAULT 1;
ALTER TABLE review_again_decisions ADD COLUMN tier TEXT NOT NULL DEFAULT '';
ALTER TABLE review_again_decisions ADD COLUMN worst REAL NOT NULL DEFAULT 0;
ALTER TABLE review_again_decisions ADD COLUMN worst_all REAL NOT NULL DEFAULT 0;
ALTER TABLE review_again_decisions ADD COLUMN fixes_mean REAL NOT NULL DEFAULT 0;
ALTER TABLE review_again_decisions ADD COLUMN novel INTEGER NOT NULL DEFAULT 0;
ALTER TABLE review_again_decisions ADD COLUMN repeats INTEGER NOT NULL DEFAULT 0;
ALTER TABLE review_again_decisions ADD COLUMN yield_p REAL NOT NULL DEFAULT -1;

-- Each scored finding of a round: the reviewer's own severity label, the
-- decision model's expected score for it (taken at its word), the two
-- combined (severity), how far the session's mark says it held (hold: 1
-- fixed or confirmed, 0.5 left open, 0 rejected), and the earlier round's
-- finding it repeats, if any. The label and the model's score side by side
-- are how far the two raters agree.
CREATE TABLE review_finding_scores (
    finding_id  INTEGER PRIMARY KEY REFERENCES code_audit_findings(id) ON DELETE CASCADE,
    round_id    INTEGER NOT NULL REFERENCES work_log(id) ON DELETE CASCADE,
    label       TEXT    NOT NULL DEFAULT '',
    model_score REAL    NOT NULL DEFAULT 0,
    severity    REAL    NOT NULL DEFAULT 0,
    hold        REAL    NOT NULL DEFAULT 0,
    repeat_of   INTEGER REFERENCES code_audit_findings(id) ON DELETE SET NULL,
    probs       TEXT    NOT NULL DEFAULT '{}',
    created_at  INTEGER NOT NULL
);
CREATE INDEX review_finding_scores_round ON review_finding_scores(round_id);
