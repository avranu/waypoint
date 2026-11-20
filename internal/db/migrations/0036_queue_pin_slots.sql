-- Queue pins hold a place (WP-87): agent_order was a rank among pins, all
-- of which went before the automatic order. It is now the place (1 = next)
-- a person gave the task in the queue; everything else keeps following the
-- automatic order around it. Existing pins were first, in rank order, so
-- their places are 1, 2, 3, ...: the queue reads the same as before.
CREATE TEMP TABLE pin_slots AS
  SELECT id, row_number() OVER (ORDER BY agent_order, id) AS slot FROM issues WHERE agent_order IS NOT NULL;
UPDATE issues SET agent_order = (SELECT slot FROM pin_slots WHERE pin_slots.id = issues.id) WHERE agent_order IS NOT NULL;
DROP TABLE pin_slots;
