-- The agent kit (WP-ADR2 §4): a repo laid out as a Claude Code plugin
-- marketplace, with kit.toml saying which plugins and notes go to which
-- repos. One kit per instance for now; a row per kit repo so more can come.
CREATE TABLE agent_kits (
    id          INTEGER PRIMARY KEY,
    repo_id     INTEGER NOT NULL UNIQUE REFERENCES project_repos(id) ON DELETE CASCADE,
    -- The last good read: the default branch, its head and the parsed kit
    -- (JSON). A broken kit.toml never replaces it.
    branch      TEXT    NOT NULL DEFAULT '',
    commit_sha  TEXT    NOT NULL DEFAULT '',
    kit         TEXT    NOT NULL DEFAULT '',
    good_at     INTEGER,
    -- The last read attempt, the head it saw and what was wrong with it.
    read_at     INTEGER,
    read_sha    TEXT    NOT NULL DEFAULT '',
    errors      TEXT    NOT NULL DEFAULT '[]',
    created_at  INTEGER NOT NULL
);

-- What each checkout last reported after `wp kit sync`: one row per
-- person, machine and path.
CREATE TABLE kit_reports (
    id          INTEGER PRIMARY KEY,
    user_id     INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    machine     TEXT    NOT NULL,
    path        TEXT    NOT NULL,
    repo        TEXT    NOT NULL DEFAULT '',
    kit_commit  TEXT    NOT NULL DEFAULT '',
    plugins     TEXT    NOT NULL DEFAULT '[]',
    drift       TEXT    NOT NULL DEFAULT '[]',
    wp_version  TEXT    NOT NULL DEFAULT '',
    error       TEXT    NOT NULL DEFAULT '',
    reported_at INTEGER NOT NULL,
    UNIQUE (user_id, machine, path)
);
