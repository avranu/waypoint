-- Sandbox policy (WP-418, WP-ADR28 decision 3, PL7 §12): only a person in
-- the Waypoint UI sets a sandbox's edges, and loosening one needs a fresh
-- password check.

-- When the browser session last re-entered its password (sign-in counts).
ALTER TABLE sessions ADD COLUMN reauth_at INTEGER;

-- A project's levels: '' follows the machine's default.
CREATE TABLE sandbox_projects (
    project_id        INTEGER PRIMARY KEY REFERENCES projects(id) ON DELETE CASCADE,
    level             TEXT    NOT NULL DEFAULT '',  -- vm, contained, tool, off
    interactive_level TEXT    NOT NULL DEFAULT '',  -- for sessions a person starts; '' = level
    updated_by        INTEGER REFERENCES users(id) ON DELETE SET NULL,
    updated_at        INTEGER NOT NULL
);

-- A runner machine's default level and its kill switch: paused returns the
-- machine to running sessions unsandboxed, as before sandboxing.
CREATE TABLE sandbox_machines (
    runner_id     INTEGER PRIMARY KEY REFERENCES runners(id) ON DELETE CASCADE,
    default_level TEXT    NOT NULL DEFAULT '',
    paused        INTEGER NOT NULL DEFAULT 0,
    updated_by    INTEGER REFERENCES users(id) ON DELETE SET NULL,
    updated_at    INTEGER NOT NULL
);

-- Every change, kept after its project or machine is gone.
CREATE TABLE sandbox_policy_log (
    id          INTEGER PRIMARY KEY,
    at          INTEGER NOT NULL,
    user_id     INTEGER REFERENCES users(id) ON DELETE SET NULL,
    scope       TEXT    NOT NULL,              -- project, machine
    target_id   INTEGER NOT NULL,              -- project or runner id
    target_name TEXT    NOT NULL DEFAULT '',   -- its key or name then
    field       TEXT    NOT NULL,              -- level, interactive_level, default_level, paused
    old_value   TEXT    NOT NULL DEFAULT '',
    new_value   TEXT    NOT NULL DEFAULT '',
    loosens     INTEGER NOT NULL DEFAULT 0,
    reason      TEXT    NOT NULL DEFAULT '',
    reverts     INTEGER REFERENCES sandbox_policy_log(id) ON DELETE SET NULL
);
CREATE INDEX sandbox_policy_log_target ON sandbox_policy_log(scope, target_id, id);
