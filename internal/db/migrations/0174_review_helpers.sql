-- Haiku helpers around a review's model, run by the runner in the same job
-- (WP-1320): a panel of fixed checks before the reviewer, whose leads it is
-- handed, and a check of each of the reviewer's findings against the code
-- after it.

-- helpers: what the audit asks of them, as JSON ({"panel":[{"name","ask"}],
-- "context":"…","verify":true}); '' none.
ALTER TABLE code_audits ADD COLUMN helpers TEXT NOT NULL DEFAULT '';

-- A finding's check against the code: verdict confirmed, refuted or
-- unsupported ('' not checked), with the checker's evidence (verdict_note);
-- lead: the panel lead (check.n) the reviewer says the finding came from.
ALTER TABLE code_audit_findings ADD COLUMN verdict TEXT NOT NULL DEFAULT '';
ALTER TABLE code_audit_findings ADD COLUMN verdict_note TEXT NOT NULL DEFAULT '';
ALTER TABLE code_audit_findings ADD COLUMN lead TEXT NOT NULL DEFAULT '';

-- One helper run of an audit: a panel check (check_name its name) or the
-- findings' verification (check_name 'verify'). state done, failed or
-- skipped (why in error); leads: the panel check's leads after pruning, as
-- JSON ([{"id","title","severity","path","line","problem"}]); dropped: how
-- many pruning took out; cost_usd and ms: what it took.
CREATE TABLE audit_helper_runs (
    id         INTEGER PRIMARY KEY,
    audit_id   INTEGER NOT NULL REFERENCES code_audits(id) ON DELETE CASCADE,
    check_name TEXT    NOT NULL,
    state      TEXT    NOT NULL,
    error      TEXT    NOT NULL DEFAULT '',
    leads      TEXT    NOT NULL DEFAULT '[]',
    dropped    INTEGER NOT NULL DEFAULT 0,
    cost_usd   REAL    NOT NULL DEFAULT 0,
    ms         INTEGER NOT NULL DEFAULT 0,
    created_at INTEGER NOT NULL
);
CREATE INDEX audit_helper_runs_audit ON audit_helper_runs(audit_id);
CREATE INDEX audit_helper_runs_check ON audit_helper_runs(check_name, created_at);

-- signals: how many of a decided round's findings called for another round
-- by their labels alone (againDecision.signals): new, standing, not refuted
-- by the check against the code, and at or over the stakes tier's floor.
ALTER TABLE review_again_decisions ADD COLUMN signals INTEGER NOT NULL DEFAULT 0;

-- checked: what became of the round's check of the session's marks: -1 not
-- asked (no marks to check, at the cap, or decided before it existed), 1
-- its findings scored with the round's, 0 passed over (it couldn't be
-- asked, failed, or ran past its patience). unchecked: for one passed over,
-- the rejections that would have been signals, which call for another
-- round in its place.
ALTER TABLE review_again_decisions ADD COLUMN checked INTEGER NOT NULL DEFAULT -1;
ALTER TABLE review_again_decisions ADD COLUMN unchecked INTEGER NOT NULL DEFAULT 0;

-- labels: 1 when the decision model couldn't score the round, so its
-- findings were read by their labels alone, every one taken as new. From
-- the second round on, a run sends only one such round on (WP-1320).
ALTER TABLE review_again_decisions ADD COLUMN labels INTEGER NOT NULL DEFAULT 0;
