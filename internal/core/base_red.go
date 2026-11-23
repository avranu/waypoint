package core

// Fix the base first (WP-428, WP-ADR29). When a job fails on a base's tip, every pull
// request brought up to date with it fails that job too: the front's full
// run fails, it gets a round to fix what isn't its own, and so does the next
// one, while the pull request that fixes the base waits its turn behind
// them. On 2026-11-02 one repo's main failed its browser tests and the pull
// request that fixed them was tenth in line. So a queued pull request whose
// head holds the base's tip, passes a job that fails there and fails none
// of them goes to the front of the queue, behind only one already marked
// ready. One per base tip: a second that passes too waits its turn.

import (
	"context"
	"strings"
)

// baseFailing reads the jobs that fail on pr's base's tip, as its CI last
// reported them; none while it runs or when the tip isn't known.
func (s *Service) baseFailing(ctx context.Context, pr *PullRequest) (string, map[string]bool) {
	var tip string
	if err := s.db.R.QueryRowContext(ctx, `SELECT head_sha FROM branch_activity WHERE forge_id = ? AND repo = ? COLLATE NOCASE AND branch = ?`,
		pr.ForgeID, pr.Repo, pr.BaseRef).Scan(&tip); err != nil || tip == "" {
		return "", nil
	}
	return tip, s.baseFailingAt(ctx, pr, tip)
}

// baseFailingAt is baseFailing for a tip the caller already chose.
func (s *Service) baseFailingAt(ctx context.Context, pr *PullRequest, tip string) map[string]bool {
	checks, err := s.shaChecks(ctx, pr.ForgeID, pr.Repo, tip)
	if err != nil {
		return nil
	}
	failing := map[string]bool{}
	for _, c := range dropSuperseded(checks) {
		if c.State == CheckFailed && !ownCheck(c.Name) {
			failing[checkJob(c.Name)] = true
		}
	}
	return failing
}

// checkJob is a check's job, the same on a push and on a pull request.
func checkJob(name string) string {
	if job, ok := jobOf(name); ok {
		return job
	}
	return name
}

// checkJobName is the workflow job a check is, as the forge's job list
// (ci_jobs) names it: "ci / backend shard 1 (pull_request)" is
// "backend shard 1".
func checkJobName(name string) string {
	job := checkJob(name)
	if _, j, ok := strings.Cut(job, " / "); ok {
		return j
	}
	return job
}

// shaChecks reads the checks Waypoint has stored for a commit.
func (s *Service) shaChecks(ctx context.Context, forgeID int64, repo, sha string) ([]CommitCheck, error) {
	rows, err := s.db.R.QueryContext(ctx, `SELECT c.name, c.state, c.description FROM ci_checks c JOIN ci_heads h ON h.id = c.head_id
		WHERE h.forge_id = ? AND h.repo = ? COLLATE NOCASE AND h.sha = ?`, forgeID, repo, sha)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CommitCheck
	for rows.Next() {
		var c CommitCheck
		if err := rows.Scan(&c.Name, &c.State, &c.Description); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// fixesBase: pr's head passes (ran, not skipped) a job of failing and fails
// none of them, and holds the base's tip, so it passed on top of what fails.
func (s *Service) fixesBase(ctx context.Context, pr *PullRequest, tip string, failing map[string]bool) bool {
	checks, err := s.shaChecks(ctx, pr.ForgeID, pr.Repo, pr.HeadSHA)
	if err != nil {
		return false
	}
	passed := false
	for _, c := range dropSuperseded(checks) {
		if !failing[checkJob(c.Name)] {
			continue
		}
		switch {
		case c.State == CheckFailed, c.State == CheckPending:
			return false
		case c.State == CheckSuccess && !skippedCheck(c):
			passed = true
		}
	}
	if !passed {
		return false
	}
	key := "holds:" + tip + ":" + pr.HeadSHA
	if v, ok := s.baseRed.Load(key); ok {
		return v.(bool)
	}
	cl, ok := s.Forges.(ForgeUpToDate)
	if !ok {
		return false
	}
	f, err := s.forgeByID(ctx, pr.ForgeID)
	if err != nil || f.token == "" {
		return false
	}
	holds, err := cl.Contains(ctx, f, pr.Repo, tip, pr.HeadSHA)
	if err != nil {
		return false
	}
	s.baseRed.Store(key, holds)
	return holds
}
