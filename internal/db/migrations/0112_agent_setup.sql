-- Agent setup (WP-208, WP-ADR44): the files agents read in every attached
-- repo and on every machine, the shared instructions Waypoint keeps in each
-- repo's root instruction file, and the pull requests that put them there.

-- Each attached repo's agent files as last read from the forge, at the head
-- of its default branch.
CREATE TABLE agent_repo_scans (
    forge_id   INTEGER NOT NULL REFERENCES forges(id) ON DELETE CASCADE,
    repo       TEXT    NOT NULL COLLATE NOCASE,
    branch     TEXT    NOT NULL DEFAULT '',
    head       TEXT    NOT NULL DEFAULT '',
    scanned_at INTEGER,
    error      TEXT    NOT NULL DEFAULT '',
    PRIMARY KEY (forge_id, repo)
);

CREATE TABLE agent_repo_files (
    forge_id  INTEGER NOT NULL REFERENCES forges(id) ON DELETE CASCADE,
    repo      TEXT    NOT NULL COLLATE NOCASE,
    path      TEXT    NOT NULL,
    kind      TEXT    NOT NULL,
    content   TEXT    NOT NULL DEFAULT '',
    size      INTEGER NOT NULL DEFAULT 0,
    truncated INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (forge_id, repo, path)
);

-- Every version of the shared instructions an admin saved; the highest is
-- current. Text '' takes the block out of the repos.
CREATE TABLE agent_shared_versions (
    version  INTEGER PRIMARY KEY,
    text     TEXT    NOT NULL,
    saved_by INTEGER REFERENCES users(id) ON DELETE SET NULL,
    saved_at INTEGER NOT NULL
);

-- The pull requests Waypoint opened on a repo's agent files: the shared
-- block ('shared', with the version it carries) or a person's edit
-- ('edit'). state: open, merged, closed (unmerged), failed (never opened).
CREATE TABLE agent_file_prs (
    id         INTEGER PRIMARY KEY,
    forge_id   INTEGER NOT NULL REFERENCES forges(id) ON DELETE CASCADE,
    repo       TEXT    NOT NULL COLLATE NOCASE,
    kind       TEXT    NOT NULL,
    version    INTEGER NOT NULL DEFAULT 0,
    paths      TEXT    NOT NULL DEFAULT '[]',
    branch     TEXT    NOT NULL DEFAULT '',
    number     INTEGER NOT NULL DEFAULT 0,
    url        TEXT    NOT NULL DEFAULT '',
    state      TEXT    NOT NULL DEFAULT 'open',
    error      TEXT    NOT NULL DEFAULT '',
    opened_by  INTEGER REFERENCES users(id) ON DELETE SET NULL,
    opened_at  INTEGER NOT NULL,
    closed_at  INTEGER
);
CREATE INDEX agent_file_prs_repo ON agent_file_prs(forge_id, repo, kind, state);

-- A machine's home-folder setup as `wp` last reported it: its global
-- instruction files, agents, skills, commands, settings (secrets left out)
-- and Waypoint's own checks there.
CREATE TABLE machine_reports (
    user_id     INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    machine     TEXT    NOT NULL,
    os          TEXT    NOT NULL DEFAULT '',
    wp_version  TEXT    NOT NULL DEFAULT '',
    files       TEXT    NOT NULL DEFAULT '[]',
    checks      TEXT    NOT NULL DEFAULT '[]',
    reported_at INTEGER NOT NULL,
    PRIMARY KEY (user_id, machine)
);
