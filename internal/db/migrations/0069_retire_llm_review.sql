-- forge-checks' llm-review account reviews nothing since Waypoint took
-- reviews over, so a repo that expects it waits the whole review wait on
-- every head (WP-187). Drop it from the repos that name it; a list left
-- empty goes back to the forge's default.
UPDATE project_repos SET external_reviewers = (
    SELECT CASE WHEN count(*) = 0 THEN NULL ELSE json_group_array(value) END
    FROM json_each(project_repos.external_reviewers) WHERE lower(value) <> 'llm-review')
WHERE external_reviewers IS NOT NULL AND json_valid(external_reviewers)
  AND EXISTS (SELECT 1 FROM json_each(project_repos.external_reviewers) WHERE lower(value) = 'llm-review');
