-- WP-752: since reviews before the pull request (WP-688) a session's run
-- stays open through wp ship, and the Stop hook that pauses the clock raced
-- the one that schedules the review. When the pause won, the round was
-- swept as "autopilot moved on without it" and the task sat In Progress
-- with its branch pushed and no pull request. Bring those back: the
-- watchdog (reviveHeldRounds) runs them once the run is open again.
--
-- Only rounds nothing has happened after: the session is still open, its
-- run was paused and is its newest, nobody prompted it since, and the task
-- is open with no pull request.
UPDATE work_log SET state = 'pending', reason = '', ended_at = NULL
WHERE kind = 'review' AND state = 'skipped' AND reason = 'autopilot moved on without it' AND before_pr != ''
  AND EXISTS (SELECT 1 FROM agent_sessions x WHERE x.id = work_log.session_id AND x.ended_at IS NULL)
  AND EXISTS (SELECT 1 FROM agent_runs r WHERE r.id = work_log.run_id AND r.outcome = 'paused')
  AND NOT EXISTS (SELECT 1 FROM agent_runs o WHERE o.session_id = work_log.session_id AND o.id > work_log.run_id)
  AND NOT EXISTS (SELECT 1 FROM session_events e WHERE e.session_id = work_log.session_id AND e.kind = 'prompt' AND e.created_at > work_log.updated_at)
  AND EXISTS (SELECT 1 FROM issues i WHERE i.id = work_log.issue_id AND i.resolved_at IS NULL)
  AND NOT EXISTS (SELECT 1 FROM external_links l WHERE l.issue_id = work_log.issue_id AND l.kind = 'pull');

-- A paused run with a review still to come is waiting on Waypoint, not on
-- its person (one per task and agent, the newest, and only if none is open).
UPDATE agent_runs SET ended_at = NULL, outcome = ''
WHERE outcome = 'paused'
  AND id IN (SELECT max(run_id) FROM work_log WHERE kind = 'review' AND before_pr != '' AND state IN ('pending', 'running') AND run_id IS NOT NULL GROUP BY issue_id)
  AND NOT EXISTS (SELECT 1 FROM agent_runs o WHERE o.issue_id = agent_runs.issue_id AND o.agent = agent_runs.agent AND o.ended_at IS NULL);
