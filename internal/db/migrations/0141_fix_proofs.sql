-- Proven fixes (WP-709): a regression test that passes on the buggy code
-- proves nothing. A runner runs the Go tests a traced bug fix added or
-- changed on the fix and on the code before it (with the fix's test files)
-- and uploads one row per fix. outcome (internal/codeindex ProveFix):
-- 'proven' (a test failed without the fix and passed with it), 'passes'
-- (they pass without it too), 'no_build' (they don't build without it),
-- 'head_fails', 'no_test', 'unsupported' (not Go) or 'none' (note says why).
-- When the trace blamed a change, intro_check says what the proving tests
-- did there: 'confirmed' (fail on it, pass before it), 'passes_at_intro',
-- 'fails_before' or 'no_build'. A confirmed blame raises that introducer's
-- confidence to 1; a refuted one drops it to 0.
CREATE TABLE fix_proofs (
    pr_id       INTEGER PRIMARY KEY REFERENCES pull_requests(id) ON DELETE CASCADE,
    outcome     TEXT    NOT NULL,
    fix_sha     TEXT    NOT NULL DEFAULT '',
    base_sha    TEXT    NOT NULL DEFAULT '',
    tests       TEXT    NOT NULL DEFAULT '[]', -- dir.TestName run
    failed      TEXT    NOT NULL DEFAULT '[]', -- of those, failed without the fix
    intro_merge TEXT    NOT NULL DEFAULT '',
    intro_check TEXT    NOT NULL DEFAULT '',
    note        TEXT    NOT NULL DEFAULT '',
    machine     TEXT    NOT NULL DEFAULT '',
    seconds     INTEGER NOT NULL DEFAULT 0,
    proved_at   INTEGER NOT NULL
);
