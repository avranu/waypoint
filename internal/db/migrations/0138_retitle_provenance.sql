-- The title model's edit made an agent's task read as a person's (WP-850):
-- it cleared text_by and text_agent. Give those tasks their author back.
-- A task qualifies when an agent filed it, nobody is named as its text's
-- author now, the title model made its last text edit, and no person ever
-- edited its text. Its review stays as it is. origin_issue_id can't be put
-- back: no event records the session a task was filed from.
CREATE TEMP TABLE wp850_restored AS
SELECT i.id FROM issues i
WHERE i.reporter_agent != '' AND i.text_agent = '' AND i.text_by IS NULL
    AND (SELECT coalesce(json_extract(e.data, '$.via'), '') FROM events e
        WHERE e.issue_id = i.id AND e.kind = 'issue.updated' AND e.field IN ('title', 'description')
        ORDER BY e.id DESC LIMIT 1) LIKE 'AI title%'
    AND NOT EXISTS (SELECT 1 FROM events e
        WHERE e.issue_id = i.id AND e.kind = 'issue.updated' AND e.field IN ('title', 'description')
            AND e.actor_id IS NOT NULL AND coalesce(json_extract(e.data, '$.via'), '') = '');

UPDATE issues SET text_by = reporter_id, text_agent = reporter_agent
WHERE id IN (SELECT id FROM wp850_restored);

-- Screen them and the tasks written from them again (the sweep takes a To Do
-- task whose verdict is older than its last change; a closed one is screened
-- when it is reopened): a verdict from before this read none of their text,
-- and those chains are now deeper. Agents get a task only on a verdict for
-- its text as it reads now (the screen's hash), so a stale one releases
-- nothing.
WITH RECURSIVE below(id, n) AS (
    SELECT id, 0 FROM wp850_restored
    UNION SELECT i.id, below.n + 1 FROM issues i JOIN below ON i.origin_issue_id = below.id WHERE below.n < 20)
UPDATE task_safety SET updated_at = (SELECT i.updated_at - 1 FROM issues i WHERE i.id = task_safety.issue_id)
WHERE issue_id IN (SELECT id FROM below);

DROP TABLE wp850_restored;
