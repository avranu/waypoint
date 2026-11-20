-- Forge text agents may have written (WP-282, WP-ADR13). Agents can hold a
-- person's forge credentials (a git credential helper, a gh or tea token)
-- and write on the forge as them, so text from a person's login counts as
-- agent-written and unreviewed, like any text an agent writes here, unless
-- an admin says only that person writes as it (person_only = 1).
ALTER TABLE forge_people ADD COLUMN person_only INTEGER NOT NULL DEFAULT 0;

-- What was let in before this was taken as a person's own text. Its login
-- isn't kept, so it counts as written through some forge login: unreviewed
-- until a person reviews it, and screened before agents take it (WP-ADR10).
UPDATE issues SET text_agent = 'forge', reviewed_by = NULL, reviewed_at = NULL
WHERE outside = 0 AND text_agent = '' AND text_by IS NULL
    AND id IN (SELECT issue_id FROM external_links WHERE mirror = 1);
UPDATE comments SET agent = 'forge/' || external_author, reviewed_by = NULL, reviewed_at = NULL
WHERE agent = '' AND author_id IS NULL AND external_author != ''
    AND id IN (SELECT comment_id FROM comment_links WHERE origin = 1);
