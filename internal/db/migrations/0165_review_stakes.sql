-- The stakes tier each review round was given (WP-1248, WP-1250, WP-ADR121).
-- A round decides its tier once, the first time it asks for its audit, and
-- keeps it: the stakes report (wp worklog stakes) shows what each round was
-- actually given, under the rule of the day, rather than recomputing old
-- rounds under a newer rule. source says where the tier came from: the
-- round's triage facts (rule), the task's earlier round (inherited: a second
-- round has no triage of its own), or neither (none: unknown, the old
-- handling).
CREATE TABLE review_stakes (
    round_id   INTEGER PRIMARY KEY REFERENCES work_log(id) ON DELETE CASCADE,
    issue_id   INTEGER NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
    rule       TEXT    NOT NULL,             -- stakes-v2, ...
    tier       TEXT    NOT NULL,             -- high, elevated, normal, unknown
    why        TEXT    NOT NULL DEFAULT '',
    bar        TEXT    NOT NULL DEFAULT '',  -- the audit's quality bar ('' none)
    source     TEXT    NOT NULL DEFAULT 'rule',
    created_at INTEGER NOT NULL
);
CREATE INDEX review_stakes_issue ON review_stakes(issue_id, round_id);
