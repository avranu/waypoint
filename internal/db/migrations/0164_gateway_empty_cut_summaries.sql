-- 0163 marked gateway answers that raised no findings (answered = 2) from
-- their audit's summary, where a note says findings were left out below a
-- severity floor or cap. A summary is stored cut at 2000 characters, so a
-- long one may have lost that note (WP-1253): those not yet reported go
-- back to unmarked, rather than tell OpenLLMs an answer found nothing when
-- it may have raised findings.
UPDATE gateway_tasks SET answered = 1
WHERE answered = 2 AND reported_at IS NULL
  AND EXISTS (SELECT 1 FROM code_audits a WHERE a.id = gateway_tasks.audit_id AND length(a.summary) >= 1950);
-- Neither the summary nor the findings table shows findings dropped as
-- blank (no title and no problem), or free text that says more than "no
-- findings". Only an answer stored as JSON with a findings list and no
-- finding in it stays marked, and so does text that is only "No findings."
-- (WP-1253).
UPDATE gateway_tasks SET answered = 1
WHERE answered = 2 AND reported_at IS NULL
  AND EXISTS (SELECT 1 FROM code_audits a WHERE a.id = gateway_tasks.audit_id
              AND ((a.answer NOT LIKE '%"findings"%'
                     AND upper(trim(a.answer, ' ' || char(10) || '.!*_`')) <> 'NO FINDINGS')
                    OR a.answer LIKE '%"title"%' OR a.answer LIKE '%"problem"%'));
