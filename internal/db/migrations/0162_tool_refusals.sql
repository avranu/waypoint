-- Refused tool calls are logged, not asked (WP-232, WP-ADR115). When Claude
-- Code's auto-mode classifier refuses a call, the PermissionDenied hook used
-- to file a permission request; the agent had usually moved on by the time a
-- person saw it, and asked again itself if it still needed the call. Each
-- refusal is now a row here, read under Agents → Audit log → Refused calls
-- and on the session's timeline, and nothing waits on a person.
--
-- A repeat of the same call in the same session within ten minutes adds to
-- repeats and moves last_at. ran_at: the same call later ran in that
-- session (a person allowed it in the terminal). command: what a person
-- recognises (the Bash command, the file path), kept whole; input: the
-- tool's whole input as JSON, '' when it was too large to keep.
CREATE TABLE tool_refusals (
    id          INTEGER PRIMARY KEY,
    user_id     INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    project_id  INTEGER REFERENCES projects(id) ON DELETE CASCADE,
    session_id  INTEGER REFERENCES agent_sessions(id) ON DELETE SET NULL,
    issue_id    INTEGER REFERENCES issues(id) ON DELETE SET NULL,
    agent       TEXT    NOT NULL DEFAULT '',
    source      TEXT    NOT NULL DEFAULT 'classifier',
    tool        TEXT    NOT NULL,
    command     TEXT    NOT NULL DEFAULT '',
    input       TEXT    NOT NULL DEFAULT '',
    reason      TEXT    NOT NULL DEFAULT '',
    tool_use_id TEXT    NOT NULL DEFAULT '',
    dedupe_key  TEXT    NOT NULL,
    repeats     INTEGER NOT NULL DEFAULT 1,
    created_at  INTEGER NOT NULL,
    last_at     INTEGER NOT NULL,
    ran_at      INTEGER,
    request_id  INTEGER REFERENCES requests(id) ON DELETE SET NULL, -- the permission request it was, before WP-232
    -- An allow rule a person picked from this refusal (WP-233, WP-ADR114),
    -- added to the session repo's .claude/settings.json by pull request:
    -- rule is set once it is in effect or on its way (rule_pr opens it, or
    -- the file already had it); rule_reason says why not, or that it was there.
    rule        TEXT    NOT NULL DEFAULT '',
    rule_by     INTEGER REFERENCES users(id) ON DELETE SET NULL,
    rule_at     INTEGER,
    rule_pr     TEXT    NOT NULL DEFAULT '',
    rule_reason TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX tool_refusals_at ON tool_refusals(last_at);
CREATE INDEX tool_refusals_session ON tool_refusals(session_id, dedupe_key, last_at);
CREATE INDEX tool_refusals_project ON tool_refusals(project_id, last_at);

-- The refused calls filed as permission requests so far, so the log starts
-- with them. Their requests stay as they were.
INSERT INTO tool_refusals(user_id, project_id, session_id, issue_id, agent, tool, command, input, reason, tool_use_id, dedupe_key, created_at, last_at, ran_at, request_id)
SELECT r.user_id, r.project_id, r.session_id, r.issue_id, r.agent,
       coalesce(json_extract(r.payload, '$.tool.name'), ''),
       -- The command (or path, URL…) as toolSummary picks it, from the
       -- input, which the payload kept whole or not at all; the body's fence
       -- only without it, as a command with a fence of its own (a PR body)
       -- would end at that.
       CASE WHEN json_type(r.payload, '$.tool.input') = 'object' THEN coalesce(
                     CASE WHEN json_type(r.payload, '$.tool.input.command') = 'text' AND trim(json_extract(r.payload, '$.tool.input.command'), ' ' || char(9, 10, 13)) != '' THEN trim(json_extract(r.payload, '$.tool.input.command'), ' ' || char(9, 10, 13)) END,
                     CASE WHEN json_type(r.payload, '$.tool.input.file_path') = 'text' AND trim(json_extract(r.payload, '$.tool.input.file_path'), ' ' || char(9, 10, 13)) != '' THEN trim(json_extract(r.payload, '$.tool.input.file_path'), ' ' || char(9, 10, 13)) END,
                     CASE WHEN json_type(r.payload, '$.tool.input.path') = 'text' AND trim(json_extract(r.payload, '$.tool.input.path'), ' ' || char(9, 10, 13)) != '' THEN trim(json_extract(r.payload, '$.tool.input.path'), ' ' || char(9, 10, 13)) END,
                     CASE WHEN json_type(r.payload, '$.tool.input.url') = 'text' AND trim(json_extract(r.payload, '$.tool.input.url'), ' ' || char(9, 10, 13)) != '' THEN trim(json_extract(r.payload, '$.tool.input.url'), ' ' || char(9, 10, 13)) END,
                     CASE WHEN json_type(r.payload, '$.tool.input.pattern') = 'text' AND trim(json_extract(r.payload, '$.tool.input.pattern'), ' ' || char(9, 10, 13)) != '' THEN trim(json_extract(r.payload, '$.tool.input.pattern'), ' ' || char(9, 10, 13)) END,
                     CASE WHEN json_type(r.payload, '$.tool.input.query') = 'text' AND trim(json_extract(r.payload, '$.tool.input.query'), ' ' || char(9, 10, 13)) != '' THEN trim(json_extract(r.payload, '$.tool.input.query'), ' ' || char(9, 10, 13)) END,
                     CASE WHEN json_type(r.payload, '$.tool.input.description') = 'text' AND trim(json_extract(r.payload, '$.tool.input.description'), ' ' || char(9, 10, 13)) != '' THEN trim(json_extract(r.payload, '$.tool.input.description'), ' ' || char(9, 10, 13)) END, '')
            WHEN r.body LIKE '```%' THEN trim(substr(r.body, 5, instr(substr(r.body, 5), char(10) || '```') - 1), char(10)) ELSE '' END,
       coalesce(json_extract(r.payload, '$.tool.input'), ''),
       coalesce(json_extract(r.payload, '$.tool.reason'), ''),
       r.tool_use_id, r.dedupe_key, r.created_at, r.created_at,
       CASE WHEN r.decision = 'session' THEN r.decided_at END,
       r.id
FROM requests r
WHERE r.kind = 'permission' AND r.tool_use_id != '' AND json_extract(r.payload, '$.tool.name') IS NOT NULL
ORDER BY r.id;
