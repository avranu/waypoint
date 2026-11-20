-- What people do with related-issue suggestions (WP-958): each one a
-- person was shown, with the features it was scored on then, and what
-- became of it. outcome is 'accepted' (the pair was related by choice
-- after it was shown), 'dismissed' (the person said no), or NULL: no
-- decision yet, which counts as ignored once a day has passed.
CREATE TABLE relation_suggestions (
    id            INTEGER PRIMARY KEY,
    issue_id      INTEGER NOT NULL REFERENCES issues(id) ON DELETE CASCADE, -- the issue it was suggested on
    other_id      INTEGER NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
    kind          TEXT    NOT NULL,
    source        TEXT    NOT NULL, -- text, forge, title, words, concepts
    confidence    TEXT    NOT NULL,
    shown_by      INTEGER REFERENCES users(id) ON DELETE SET NULL,
    shown_at      INTEGER NOT NULL,
    times_shown   INTEGER NOT NULL DEFAULT 1,
    word          REAL    NOT NULL, -- tf-idf cosine
    concept       REAL,             -- concept cosine; NULL without a model
    same_project  INTEGER NOT NULL,
    shared_parent INTEGER NOT NULL,
    shared_goal   INTEGER NOT NULL,
    age_days      REAL    NOT NULL, -- days between the two being filed
    source_done   INTEGER NOT NULL, -- the issue it was suggested on is done or canceled
    blend         REAL    NOT NULL, -- the fixed score (WP-ADR91)
    model_p       REAL,             -- the learned ranker's score, when it chose
    outcome       TEXT,
    decided_at    INTEGER,
    decided_by    INTEGER REFERENCES users(id) ON DELETE SET NULL,
    UNIQUE (issue_id, other_id)
);
CREATE INDEX relation_suggestions_other ON relation_suggestions(other_id);
CREATE INDEX relation_suggestions_shown ON relation_suggestions(shown_at);

-- Each nightly fit of the ranker, adopted or not, with the figures it was
-- judged on, so the latest adopted one survives a restart.
CREATE TABLE relation_ranker_fits (
    id         INTEGER PRIMARY KEY,
    fitted_at  INTEGER NOT NULL,
    train      INTEGER NOT NULL,
    test       INTEGER NOT NULL,
    accepted   INTEGER NOT NULL,
    dismissed  INTEGER NOT NULL,
    ignored    INTEGER NOT NULL,
    auc_model  REAL,
    auc_blend  REAL,
    wins       REAL,               -- share of paired resamples the model won
    adopted    INTEGER NOT NULL,
    model      TEXT    NOT NULL DEFAULT '', -- JSON weights, when adopted
    note       TEXT    NOT NULL DEFAULT ''
);
