-- Token permissions (WP-111): a token can carry a JSON list of the
-- permissions it has ("issues.read", "pages.write", ...). NULL keeps
-- today's meaning: unrestricted, or read-only when read_only is set.
ALTER TABLE api_tokens ADD COLUMN permissions TEXT;

-- What a permission token was refused, counted per permission, so its
-- owner can decide whether to grant it.
CREATE TABLE api_token_refusals (
    token_id   INTEGER NOT NULL REFERENCES api_tokens(id) ON DELETE CASCADE,
    permission TEXT NOT NULL,
    count      INTEGER NOT NULL DEFAULT 0,
    last_at    INTEGER NOT NULL,
    what       TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (token_id, permission)
);

-- An automation run a permission token set off keeps the token's
-- permissions, durable steps included, so it can do no more than the token.
ALTER TABLE automation_runs ADD COLUMN permissions TEXT;
