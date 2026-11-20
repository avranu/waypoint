-- Merge automatically (WP-133, ADR2 §3): a repo's policy for merging its
-- pull requests without a person.
--
-- auto_merge arms every open, non-draft pull request Waypoint sees (not
-- forks, release PRs, or ones labelled do-not-merge or wip). On for every
-- repo, and for repos attached later.
ALTER TABLE project_repos ADD COLUMN auto_merge INTEGER NOT NULL DEFAULT 1;
-- Status contexts that must have passed on the head, on top of every check
-- passing (a JSON list).
ALTER TABLE project_repos ADD COLUMN required_checks TEXT NOT NULL DEFAULT '[]';
-- Reviews settle when every expected reviewer posted on the head, or
-- review_wait_minutes passed since the head was first seen; and nothing new
-- was posted for quiet_minutes.
ALTER TABLE project_repos ADD COLUMN review_wait_minutes INTEGER NOT NULL DEFAULT 30;
ALTER TABLE project_repos ADD COLUMN quiet_minutes INTEGER NOT NULL DEFAULT 10;
-- Bots outside Waypoint whose reviews are expected: forge logins, or status
-- contexts (with a "/"). NULL is the forge's default: llm-review and
-- forge-checks/codeql on Forgejo, none elsewhere.
ALTER TABLE project_repos ADD COLUMN external_reviewers TEXT;

-- The repo the head branch lives in (a fork's differs); '' when unknown.
ALTER TABLE pull_requests ADD COLUMN head_repo TEXT NOT NULL DEFAULT '';
-- When Waypoint first saw the current head: the review wait counts from it.
ALTER TABLE pull_requests ADD COLUMN head_at INTEGER;
UPDATE pull_requests SET head_at = coalesce(
    (SELECT max(e.created_at) FROM pr_events e WHERE e.pr_id = pull_requests.id AND e.sha = pull_requests.head_sha AND e.kind IN ('opened','pushed')),
    updated_at)
WHERE head_sha != '';
-- 1: a person took merge when ready off, so Merge automatically leaves it
-- alone. 2: it was open before Merge automatically existed, so the setting
-- arms it at its next push (or when a person turns the setting on), not on
-- the deploy: pull requests left open on purpose don't merge by surprise.
ALTER TABLE pull_requests ADD COLUMN arm_declined INTEGER NOT NULL DEFAULT 0;
UPDATE pull_requests SET arm_declined = 2 WHERE state IN ('open','draft') AND armed_at IS NULL;
-- When an armed pull request waiting on reviews or quiet is looked at again.
ALTER TABLE pull_requests ADD COLUMN merge_wake_at INTEGER;
CREATE INDEX pull_requests_wake ON pull_requests(merge_wake_at) WHERE merge_wake_at IS NOT NULL;
