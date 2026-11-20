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
	"fmt"
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
	checks, err := s.shaChecks(ctx, pr.ForgeID, pr.Repo, tip)
	if err != nil {
		return "", nil
	}
	failing := map[string]bool{}
	for _, c := range dropSuperseded(checks) {
		if c.State == CheckFailed && !ownCheck(c.Name) {
			failing[checkJob(c.Name)] = true
		}
	}
	return tip, failing
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

// fixFirst puts pr, queued, at the front of its queue when it fixes what
// fails on its base's tip. Under the queue's lock.
func (s *Service) fixFirst(ctx context.Context, pr *PullRequest) {
	if pr.QueuedAt == nil || pr.Outside || queueHolder(pr) || pr.HeadSHA == "" {
		return
	}
	tip, failing := s.baseFailing(ctx, pr)
	if len(failing) == 0 {
		return
	}
	claim := fmt.Sprintf("first:%d/%s/%s@%s", pr.ForgeID, strings.ToLower(pr.Repo), pr.BaseRef, tip)
	if v, ok := s.baseRed.Load(claim); ok && v.(int64) != pr.ID {
		if other, err := s.prByID(ctx, s.db.R, v.(int64)); err == nil {
			if queued, qerr := s.inQueue(ctx, other); qerr != nil || queued {
				return // another one fixes it and went first, or can't tell: leave it for the next look
			}
		}
	}
	if !s.fixesBase(ctx, pr, tip, failing) {
		return
	}
	var first int64
	if err := s.db.R.QueryRowContext(ctx, `SELECT coalesce(min(coalesce(p.queued_at, p.armed_at)), 0) FROM pull_requests p
		WHERE p.forge_id = ? AND p.repo = ? COLLATE NOCASE AND p.base_ref = ? AND p.id != ? AND p.state IN ('open','draft') AND p.armed_at IS NOT NULL`,
		pr.ForgeID, pr.Repo, pr.BaseRef, pr.ID).Scan(&first); err != nil {
		return
	}
	s.baseRed.Store(claim, pr.ID)
	if first == 0 || first > pr.QueuedAt.UnixMilli() {
		return // first already
	}
	if _, err := s.db.W.ExecContext(ctx, `UPDATE pull_requests SET queued_at = ? WHERE id = ? AND queued_at IS NOT NULL`, first-1, pr.ID); err != nil {
		s.log.WarnContext(ctx, "put a base fix first", "pr", pr.ID, "err", err)
		return
	}
	at := msTime(first - 1)
	pr.QueuedAt = &at
	s.log.Info("a base fix goes first in its merge queue", "pr", pr.ID, "repo", pr.Repo, "base", pr.BaseRef, "tip", short(tip))
}
