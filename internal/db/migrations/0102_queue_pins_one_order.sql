-- Queue pins have one order across every project's queue (WP-494, WP-ADR35). A
-- pin's agent_order counted 1, 2, 3 in the queue it was set in, so where
-- two projects' queues meet (the queue of every project, an agent's next
-- task) pins from both took turns by those colliding numbers. The pins
-- are numbered once, all together: each project's pins keep their order,
-- one project after another, the project whose queue was reordered most
-- recently first.
CREATE TEMP TABLE pin_order AS
    SELECT i.id, row_number() OVER (
        ORDER BY coalesce(l.at, 0) DESC, i.project_id, i.agent_ahead, i.agent_order, i.id) AS n
    FROM issues i LEFT JOIN (
        SELECT project_id, max(created_at) AS at FROM audit_log
        WHERE action IN ('queue.pin', 'queue.move') GROUP BY project_id
    ) l ON l.project_id = i.project_id
    WHERE i.agent_order IS NOT NULL;
UPDATE issues SET agent_order = (SELECT n FROM pin_order WHERE pin_order.id = issues.id) * 1024
WHERE agent_order IS NOT NULL;
DROP TABLE pin_order;

-- Adding each new issue at the top of the board halved the room between
-- two ranks until they were equal, and those issues no longer kept the
-- order they were put in. Where ranks have run out of room, they are
-- spread out again in the order the board shows them.
CREATE TEMP TABLE rank_order AS
    SELECT id, row_number() OVER (PARTITION BY project_id ORDER BY rank, id) AS n FROM issues
    WHERE project_id IN (
        SELECT project_id FROM (
            SELECT project_id, rank - lag(rank) OVER (PARTITION BY project_id ORDER BY rank, id) AS gap FROM issues)
        WHERE gap < 0.000001);
UPDATE issues SET rank = (SELECT n FROM rank_order WHERE rank_order.id = issues.id) * 1024
WHERE id IN (SELECT id FROM rank_order);
DROP TABLE rank_order;
