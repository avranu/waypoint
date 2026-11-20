-- Text from outside on pull requests (WP-281, WP-ADR14): what a forge user
-- who isn't a Waypoint person writes on a pull request (its title,
-- description and code; reviews, review comments and conversation comments)
-- reaches no agent, automation or model until a person approves it. See
-- core/outside_pulls.go.

-- 1 while a pull request from outside waits for a person: no agent sees it,
-- no bot or automation acts on it, and its title shows only once approved.
ALTER TABLE pull_requests ADD COLUMN outside INTEGER NOT NULL DEFAULT 0;
-- Who the forge says opened it (author may name the original author of a
-- mirror's copy), so a later report that doesn't say still decides alike.
ALTER TABLE pull_requests ADD COLUMN forge_author TEXT NOT NULL DEFAULT '';
UPDATE pull_requests SET forge_author = author;

-- Outside text on pull requests, as a person decides on it: the pull
-- request itself (external_id '': its title, description and head), or a
-- comment (the forge's id). Held text reaches nothing; approved text passes
-- while the forge still sends the same text (body_hash; for the pull
-- request, the same title and head); discarded text stays out.
CREATE TABLE outside_pr_text (
    id          INTEGER PRIMARY KEY,
    pr_id       INTEGER NOT NULL REFERENCES pull_requests(id) ON DELETE CASCADE,
    external_id TEXT    NOT NULL DEFAULT '',
    kind        TEXT    NOT NULL,                -- pull, inline, review, comment
    author      TEXT    NOT NULL DEFAULT '',
    url         TEXT    NOT NULL DEFAULT '',
    path        TEXT    NOT NULL DEFAULT '',
    line        INTEGER NOT NULL DEFAULT 0,
    title       TEXT    NOT NULL DEFAULT '',
    body        TEXT    NOT NULL DEFAULT '',
    body_hash   TEXT    NOT NULL DEFAULT '',
    head_sha    TEXT    NOT NULL DEFAULT '',
    state       TEXT    NOT NULL DEFAULT 'held', -- held, approved, discarded
    decided_by  INTEGER REFERENCES users(id) ON DELETE SET NULL,
    created_at  INTEGER NOT NULL,
    updated_at  INTEGER NOT NULL,
    UNIQUE (pr_id, external_id)
);
CREATE INDEX outside_pr_text_author ON outside_pr_text(author COLLATE NOCASE);

-- Pull requests recorded before this: those whose author isn't a Waypoint
-- person on its forge wait, their titles with them. Mapping the author (or
-- the first sync mapping the forge token's login) lets them in.
INSERT INTO outside_pr_text(pr_id, kind, author, url, title, head_sha, created_at, updated_at)
SELECT p.id, 'pull', p.author, p.url, p.title, p.head_sha, p.created_at, p.updated_at FROM pull_requests p
WHERE NOT EXISTS (SELECT 1 FROM forge_people fp WHERE fp.forge_id = p.forge_id AND fp.login = p.author);
UPDATE pull_requests SET outside = 1, title = ''
WHERE NOT EXISTS (SELECT 1 FROM forge_people fp WHERE fp.forge_id = pull_requests.forge_id AND fp.login = pull_requests.author);

-- Their history keeps what reviewers wrote before this; agents don't see a
-- stranger's words there (GetPullRequest), and nothing new keeps them.
-- Review comments a stranger wrote leave what agents read, as if deleted;
-- the next read of the forge holds them for a person, and one let in comes
-- back with its verdicts.
UPDATE review_comments SET body = '', gone_at = coalesce(gone_at, CAST(strftime('%s', 'now') AS INTEGER) * 1000)
WHERE kind != 'finding' AND NOT EXISTS (
    SELECT 1 FROM pull_requests p JOIN forge_people fp ON fp.forge_id = p.forge_id AND fp.login = review_comments.author WHERE p.id = review_comments.pr_id);
-- Titles of pull requests linked from issues come back once their author
-- is a person, or a person approves them.
UPDATE external_links SET title = '' WHERE kind = 'pull' AND provider IN ('github', 'forgejo', 'gitlab');
