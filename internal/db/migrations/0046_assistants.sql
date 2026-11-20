-- Assistants (WP-ADR1): a person's own Hermes agent, reached through its
-- gate's door. The key and the Hermes password stay on the server; the
-- browser only ever talks to Waypoint, which proxies for its owner.
CREATE TABLE assistants (
    id          INTEGER PRIMARY KEY,
    owner_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name        TEXT    NOT NULL,
    url         TEXT    NOT NULL,
    ca_pem      TEXT    NOT NULL DEFAULT '',
    door_key    TEXT    NOT NULL,
    username    TEXT    NOT NULL DEFAULT 'waypoint',
    password    TEXT    NOT NULL,
    -- The Waypoint token the agent itself works with (through the gate's
    -- MCP route), so its changes here show as the assistant's.
    token_id    INTEGER REFERENCES api_tokens(id) ON DELETE SET NULL,
    -- What the watcher saw last: frozen, pending approvals, reachability.
    state       TEXT    NOT NULL DEFAULT '{}',
    created_at  INTEGER NOT NULL,
    updated_at  INTEGER NOT NULL
);
CREATE INDEX assistants_owner ON assistants(owner_id);

-- Hermes sessions tied to Waypoint items: a chat started from an issue,
-- an automation's ask, or one a person linked by hand.
CREATE TABLE assistant_links (
    id           INTEGER PRIMARY KEY,
    assistant_id INTEGER NOT NULL REFERENCES assistants(id) ON DELETE CASCADE,
    session_id   TEXT    NOT NULL,
    issue_id     INTEGER REFERENCES issues(id) ON DELETE CASCADE,
    automation_id INTEGER REFERENCES automations(id) ON DELETE SET NULL,
    title        TEXT    NOT NULL DEFAULT '',
    created_by   INTEGER REFERENCES users(id) ON DELETE SET NULL,
    created_at   INTEGER NOT NULL,
    UNIQUE (assistant_id, session_id, issue_id)
);
CREATE INDEX assistant_links_issue ON assistant_links(issue_id);
CREATE INDEX assistant_links_session ON assistant_links(assistant_id, session_id);
