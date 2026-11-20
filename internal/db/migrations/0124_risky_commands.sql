-- Risky commands (WP-647, under WP-100): shell commands an agent ran that
-- delete, discard or overwrite what can't be had back, or that reach
-- production, found by `wp usage sync` in Claude Code's transcripts by
-- plain patterns. One person's own data, like the rest of usage (an admin
-- may look too). command is the line's start, redacted on the machine.
CREATE TABLE risky_commands (
    id       INTEGER PRIMARY KEY,
    user_id  INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    at       INTEGER NOT NULL, -- ms
    machine  TEXT    NOT NULL,
    session  TEXT    NOT NULL,
    repo     TEXT    NOT NULL DEFAULT '',
    model    TEXT    NOT NULL DEFAULT '',
    issue_id INTEGER REFERENCES issues(id) ON DELETE SET NULL, -- the task the session held when it was sent
    kind     TEXT    NOT NULL, -- delete, discard, force_push, push_main, branch_delete, history, sql, cluster, deploy, service, docker, disk, power, permissions, remote_script
    level    TEXT    NOT NULL, -- high, medium
    label    TEXT    NOT NULL DEFAULT '',
    command  TEXT    NOT NULL DEFAULT '',
    segment  TEXT    NOT NULL DEFAULT '', -- the risky part, when command's start doesn't hold it
    outcome  TEXT    NOT NULL, -- ran, error, refused, unknown
    source   TEXT    NOT NULL DEFAULT '' -- who refused it: classifier, hook, user, rule, safety_check
);
CREATE INDEX risky_commands_user ON risky_commands(user_id, at);
CREATE INDEX risky_commands_at ON risky_commands(at);
