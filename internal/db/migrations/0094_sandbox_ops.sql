-- What a person asked a machine's sandbox helper to do from Settings →
-- Runners → Sandbox (WP-448, WP-ADR28): remove a machine user, or finish
-- what a crash left half done. The runner picks each up on its next poll
-- with a spec signed then, hands it to the helper and reports back.
CREATE TABLE sandbox_ops (
    id           INTEGER PRIMARY KEY,
    runner_id    INTEGER NOT NULL REFERENCES runners(id) ON DELETE CASCADE,
    op           TEXT    NOT NULL,          -- remove_user, reconcile
    user_name    TEXT    NOT NULL DEFAULT '',
    user_kind    TEXT    NOT NULL DEFAULT '',
    user_key     TEXT    NOT NULL DEFAULT '',
    requested_by INTEGER REFERENCES users(id) ON DELETE SET NULL,
    requested_at INTEGER NOT NULL,
    sent_at      INTEGER,                   -- last handed to the runner
    done_at      INTEGER,
    error        TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX sandbox_ops_runner ON sandbox_ops(runner_id, id);

-- The last job each machine user was asked for: a spec names a job's
-- repositories, keyed as the helper keys its users (repo "forge/repo id",
-- org "forge/owner").
CREATE TABLE sandbox_user_jobs (
    runner_id INTEGER NOT NULL REFERENCES runners(id) ON DELETE CASCADE,
    user_key  TEXT    NOT NULL,
    kind      TEXT    NOT NULL,             -- repo, org
    job_id    INTEGER NOT NULL,
    at        INTEGER NOT NULL,
    PRIMARY KEY (runner_id, kind, user_key)
);
