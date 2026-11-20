-- Per-repo summaries of the code index (WP-182, ADR3): `wp` uploads them from
-- a checkout of the repo's default branch. Keyed by the forge's repo id, which
-- survives renames and transfers (WP-166). One summary per repo, replaced by
-- each upload.
CREATE TABLE code_summaries (
    forge_id      INTEGER NOT NULL,
    forge_repo_id INTEGER NOT NULL,
    repo          TEXT    NOT NULL,   -- the name it was uploaded under
    commit_sha    TEXT    NOT NULL,   -- HEAD the index was built at
    indexed_at    INTEGER NOT NULL,   -- when the checkout made the summary
    uploaded_at   INTEGER NOT NULL,
    uploaded_by   INTEGER NOT NULL DEFAULT 0,
    files         INTEGER NOT NULL DEFAULT 0,
    symbols       INTEGER NOT NULL DEFAULT 0,
    commits       INTEGER NOT NULL DEFAULT 0,   -- commits of history read
    languages     TEXT    NOT NULL DEFAULT '{}', -- JSON {lang: files}
    PRIMARY KEY (forge_id, forge_repo_id)
);

CREATE TABLE code_summary_files (
    forge_id      INTEGER NOT NULL,
    forge_repo_id INTEGER NOT NULL,
    path          TEXT    NOT NULL,
    lang          TEXT    NOT NULL DEFAULT '',
    size          INTEGER NOT NULL DEFAULT 0,
    lines         INTEGER NOT NULL DEFAULT 0,
    symbols       INTEGER NOT NULL DEFAULT 0,
    importers     INTEGER NOT NULL DEFAULT 0,
    commits       INTEGER NOT NULL DEFAULT 0,
    churn         REAL    NOT NULL DEFAULT 0,
    hot_pct       REAL    NOT NULL DEFAULT 0,
    hotspot       INTEGER NOT NULL DEFAULT 0,
    fixes         INTEGER NOT NULL DEFAULT 0,
    last_fix_at   INTEGER NOT NULL DEFAULT 0,
    last_change_at INTEGER NOT NULL DEFAULT 0,
    last_author   TEXT    NOT NULL DEFAULT '',
    owner         TEXT    NOT NULL DEFAULT '',
    owner_pct     REAL    NOT NULL DEFAULT 0,
    PRIMARY KEY (forge_id, forge_repo_id, path)
) WITHOUT ROWID;

CREATE TABLE code_summary_pairs (
    forge_id      INTEGER NOT NULL,
    forge_repo_id INTEGER NOT NULL,
    a             TEXT    NOT NULL,
    b             TEXT    NOT NULL,   -- a < b
    weight        REAL    NOT NULL,
    support       INTEGER NOT NULL,
    PRIMARY KEY (forge_id, forge_repo_id, a, b)
) WITHOUT ROWID;
CREATE INDEX code_summary_pairs_b ON code_summary_pairs(forge_id, forge_repo_id, b);
