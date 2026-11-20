-- Gateway audits whose answers raised no findings (WP-1230): answered = 2,
-- so the feedback sweep reports them to OpenLLMs as no_findings. Answers
-- already recorded are marked when their audit is done with no findings
-- and its summary says none were left out below a severity floor or cap
-- ("(N more findings, below what was asked for, left out.)").
UPDATE gateway_tasks SET answered = 2
WHERE answered = 1 AND audit_id IS NOT NULL AND reported_at IS NULL
  AND NOT EXISTS (SELECT 1 FROM code_audit_findings f WHERE f.audit_id = gateway_tasks.audit_id)
  AND EXISTS (SELECT 1 FROM code_audits a WHERE a.id = gateway_tasks.audit_id AND a.status = 'done'
              AND a.summary NOT LIKE '%below what was asked for, left out.)%');
