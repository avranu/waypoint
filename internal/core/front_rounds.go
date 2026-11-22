package core

import (
	"context"
	"time"
)

// frontRoundQueued is how long a round of a pull request at the front of its
// merge line may wait for a runner before it is a problem (WP-1618,
// WP-ADR149). The doctor names the runner after doctorRunnerQueued; this is
// the long wait that nobody should have to find out about by looking.
const frontRoundQueued = 20 * time.Minute

// CheckFrontRounds logs a warning for each round at the front of a merge
// line that no runner has taken for frontRoundQueued: the line waits on it.
// The message stays the same, so the rounds of one repo count as one
// problem, which ends with the first run that finds none. Every five
// minutes.
func (s *Service) CheckFrontRounds(ctx context.Context) {
	cutoff := s.now().Add(-frontRoundQueued).UnixMilli()
	rows, err := s.db.R.QueryContext(ctx, `SELECT DISTINCT owner_id FROM runner_jobs WHERE state = 'queued' AND source = ? AND created_at < ?`, babysitSource, cutoff)
	if err != nil {
		s.log.WarnContext(ctx, "front rounds: read", "err", err)
		return
	}
	var owners []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			s.log.WarnContext(ctx, "front rounds: read", "err", err)
			return
		}
		owners = append(owners, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		s.log.WarnContext(ctx, "front rounds: read", "err", err)
		return
	}
	for _, owner := range owners {
		front, err := s.queuedFrontRounds(ctx, owner)
		if err != nil {
			s.log.WarnContext(ctx, "front rounds: read", "err", err)
			continue
		}
		for _, f := range front {
			if ctx.Err() != nil {
				return
			}
			j, err := scanJob(s.db.R.QueryRowContext(ctx, `SELECT `+jobCols+` FROM runner_jobs j LEFT JOIN users cu ON cu.id = j.created_by WHERE j.id = ?`, f.id))
			if err != nil || j.State != "queued" || j.CreatedAt.UnixMilli() >= cutoff {
				continue
			}
			var repo string
			var number int
			if err := s.db.R.QueryRowContext(ctx, `SELECT p.repo, p.number FROM babysits b JOIN pull_requests p ON p.id = b.pr_id WHERE b.id = ?`, j.SourceID).Scan(&repo, &number); err != nil {
				continue
			}
			s.log.WarnContext(ctx, "front round queued", "repo", repo, "number", number, "job", j.ID,
				"queued_minutes", int(s.now().Sub(j.CreatedAt).Minutes()), "jobs_ahead", s.aheadOf(ctx, j), "why", s.queuedWhy(ctx, j))
		}
	}
}
