-- What was live when an issue was written (WP-460): the project's production
-- environment (staging where it has none) and the version and commit it ran
-- at the issue's creation, so a reader can tell what the task assumed and how
-- much has shipped since. Recorded when the issue is created; issues from
-- before this come from the deployment history.
ALTER TABLE issues ADD COLUMN written_env INTEGER REFERENCES environments(id) ON DELETE SET NULL;
ALTER TABLE issues ADD COLUMN written_version TEXT NOT NULL DEFAULT '';
ALTER TABLE issues ADD COLUMN written_commit TEXT NOT NULL DEFAULT '';

UPDATE issues SET written_env = (
    SELECT e.id FROM environments e
    WHERE e.project_id = issues.project_id AND e.kind IN ('production', 'staging')
    ORDER BY CASE e.kind WHEN 'production' THEN 0 ELSE 1 END, e.stopped_at IS NOT NULL, e.position, e.id
    LIMIT 1);

UPDATE issues SET
    written_version = coalesce((
        SELECT d.version FROM deployments d
        WHERE d.environment_id = issues.written_env AND d.status IN ('succeeded', 'detected')
          AND coalesce(d.finished_at, d.started_at) <= issues.created_at
        ORDER BY coalesce(d.finished_at, d.started_at) DESC, d.id DESC LIMIT 1), ''),
    written_commit = coalesce((
        SELECT d.commit_sha FROM deployments d
        WHERE d.environment_id = issues.written_env AND d.status IN ('succeeded', 'detected')
          AND coalesce(d.finished_at, d.started_at) <= issues.created_at
        ORDER BY coalesce(d.finished_at, d.started_at) DESC, d.id DESC LIMIT 1), '')
WHERE written_env IS NOT NULL;

-- Nothing was deployed yet when it was written: nothing to say.
UPDATE issues SET written_env = NULL WHERE written_version = '' AND written_commit = '';
