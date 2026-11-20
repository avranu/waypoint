-- Recurring mistakes (WP-710): a kind of mistake that came up three times in
-- two weeks is filed once as a task to make it a check, and what comes after
-- the check lands is counted.

-- One class per filed task. key is the file the mistakes are in, or a
-- signature ('ci:data-race', 'test:…'); words is the first instance's gist,
-- which later instances are compared with ('' for a signature). A class is
-- never filed twice: its task's status says where it stands, and a task
-- closed as Won't Do (or deleted) means nobody wants this check.
CREATE TABLE mistake_classes (
    id         INTEGER PRIMARY KEY,
    project_id INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    key        TEXT    NOT NULL,
    words      TEXT    NOT NULL DEFAULT '',
    label      TEXT    NOT NULL,
    issue_id   INTEGER REFERENCES issues(id) ON DELETE SET NULL,
    created_at INTEGER NOT NULL
);
CREATE INDEX mistake_classes_project ON mistake_classes(project_id, created_at);

-- What was counted for a class, each piece of evidence once. source is bug,
-- finding, audit, flake or test; at is when the mistake showed up (a bug's
-- filing, not its fix), so the instances after the task's close are the
-- class recurring after its check landed.
CREATE TABLE mistake_instances (
    id        INTEGER PRIMARY KEY,
    class_id  INTEGER NOT NULL REFERENCES mistake_classes(id) ON DELETE CASCADE,
    source    TEXT    NOT NULL,
    source_id TEXT    NOT NULL,
    at        INTEGER NOT NULL,
    text      TEXT    NOT NULL DEFAULT '',
    UNIQUE (source, source_id)
);
CREATE INDEX mistake_instances_class ON mistake_instances(class_id, at);
