-- The project a runner job works for (WP-418, WP-ADR28 §7.3): a job's own
-- token reaches that project and its sub-projects only. NULL for jobs that
-- work for no project; their tokens reach none.
ALTER TABLE runner_jobs ADD COLUMN project_id INTEGER REFERENCES projects(id) ON DELETE SET NULL;

-- Jobs waiting or running when this lands keep working: their project comes
-- from what queued them (an automation's agent step falls back to its
-- automation's project).
UPDATE runner_jobs SET project_id = (SELECT st.project_id FROM autopilot_steps st WHERE st.id = runner_jobs.source_id)
    WHERE source = 'autopilot' AND state IN ('queued', 'running');
UPDATE runner_jobs SET project_id = (SELECT r.project_id FROM babysits b JOIN pull_requests pr ON pr.id = b.pr_id
        JOIN project_repos r ON r.forge_id = pr.forge_id AND r.repo = pr.repo COLLATE NOCASE
        WHERE b.id = runner_jobs.source_id ORDER BY r.project_id LIMIT 1)
    WHERE source = 'babysit' AND state IN ('queued', 'running');
-- An agent step's project is that of the repository it clones, else its run's.
UPDATE runner_jobs SET project_id = COALESCE(
        (SELECT r.project_id FROM project_repos r JOIN forges f ON f.id = r.forge_id
            WHERE json_extract(runner_jobs.input, '$.repo') = rtrim(f.base_url, '/') || '/' || r.repo || '.git' COLLATE NOCASE
            ORDER BY r.project_id LIMIT 1),
        (SELECT ar.project_id FROM automation_runs ar WHERE ar.id = runner_jobs.source_id))
    WHERE source = 'automation_step' AND kind = 'agent' AND state IN ('queued', 'running');
