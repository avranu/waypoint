-- How agents use wp (WP-618): counters per wp command by the hour, read from
-- Claude Code's transcripts by `wp usage sync`. Never a prompt, an output or
-- an argument: only command names, flag names and fixed labels.
--
-- kind: file (a transcript began), bash (a Bash call), wp (head: a command
-- run), help (head), flag (head, detail: a flag name), error (head, detail:
-- its class), unknown_flag (head, detail: the flag), unknown_command (head,
-- detail: the command), follow (head, detail: what ran in the next Bash
-- calls), signal (detail: curl_forge, sleep, sleep_ci, gh_tea).
CREATE TABLE cli_hours (
    id      INTEGER PRIMARY KEY,
    user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    hour    INTEGER NOT NULL, -- ms, start of the UTC hour
    machine TEXT    NOT NULL,
    session TEXT    NOT NULL,
    repo    TEXT    NOT NULL DEFAULT '',
    model   TEXT    NOT NULL DEFAULT '',
    agent   TEXT    NOT NULL DEFAULT 'main', -- main, sub
    kind    TEXT    NOT NULL,
    head    TEXT    NOT NULL DEFAULT '',
    detail  TEXT    NOT NULL DEFAULT '',
    n       INTEGER NOT NULL DEFAULT 0
);
CREATE UNIQUE INDEX cli_hours_key ON cli_hours(user_id, hour, machine, session, repo, model, agent, kind, head, detail);
CREATE INDEX cli_hours_session ON cli_hours(user_id, session, hour);
