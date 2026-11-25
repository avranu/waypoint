package core

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"time"

	"git.jmann.me/claude/waypoint/internal/lazyre"
)

// limitRetry is how long a round waits when nothing says when the limit
// lifts: a rate limit is over by then, and a spent plan costs one quick
// failed job per try.
const limitRetry = 30 * time.Minute

// limitTries is how many rounds in a row may be dropped on a limit whose
// reset neither the error nor the owner's quota told. Matching an error's
// words can be wrong (a test about rate limits, a quota table), and a
// dropped round never climbs the ladder, so after these the next counts
// like any failed round and the pull request still reaches a person.
const limitTries = 4

// agentFailure starts the error of a run its agent's CLI ended ("claude
// reported an error: …", "codex exited with an error: …"). The runner's own
// failures (a clone, a push, a protected file) name branches and paths,
// which a limit's words can turn up in; and they come after the agent ran.
var agentFailure = lazyre.New(`^[\w-]+ (?:reported|exited with) an error: `)

// roundLimited: the round's agent stopped on a usage or rate limit.
func roundLimited(job *RunnerJob) bool {
	if job.State != "failed" {
		return false
	}
	at := agentFailure.FindStringIndex(job.Error)
	return at != nil && limitText(job.Error[at[1]:])
}

// uncountLimitedRound drops the limited round with job jobID from the
// babysit and sets when the next may go. It reports false, dropping
// nothing, once limitTries rounds in a row hit a limit nothing put a reset
// on: that one counts.
func (s *Service) uncountLimitedRound(ctx context.Context, b *Babysit, jobID int64, msg string) bool {
	i := slices.IndexFunc(b.Rounds, func(r BabysitRound) bool { return r.Job == jobID })
	if i < 0 {
		if _, err := s.db.W.ExecContext(ctx, `UPDATE babysits SET job_id = NULL, updated_at = ? WHERE id = ?`, s.nowMS(), b.ID); err != nil {
			s.log.WarnContext(ctx, "record a babysit round", "babysit", b.ID, "err", err)
		}
		return true
	}
	until, known := s.limitLifts(ctx, b.ownerID, msg)
	tries := b.limitTries
	if !known {
		if tries >= limitTries {
			s.log.Info("babysit round hit a limit again with no reset known; counted", "babysit", b.ID, "round", b.Rounds[i].N, "job", jobID, "err", excerpt(msg, 200))
			return false
		}
		tries++
	}
	r := takeBackRound(b, i)
	b.limitedUntil, b.limitedAt, b.limitTries, b.JobID = until.UnixMilli(), s.nowMS(), tries, 0
	rounds, _ := json.Marshal(b.Rounds)
	st, _ := json.Marshal(b.streaks)
	if _, err := s.db.W.ExecContext(ctx, `UPDATE babysits SET job_id = NULL, round = ?, round_sha = ?, rounds = ?, streaks = ?, limited_until = ?, limited_at = ?, limited_tries = ?, updated_at = ? WHERE id = ?`,
		b.Round, b.RoundSHA, string(rounds), string(st), b.limitedUntil, b.limitedAt, b.limitTries, s.nowMS(), b.ID); err != nil {
		s.log.WarnContext(ctx, "record a babysit round", "babysit", b.ID, "err", err)
		return true
	}
	s.log.Info("babysit round hit a usage limit; not counted", "babysit", b.ID, "round", r.N, "job", jobID, "until", until, "err", excerpt(msg, 200))
	return true
}

// restoreStreaks puts back the streaks from before round r, the last: it
// replaced them with its own failed checks (babysitRound). A babysit whose
// round went out before prev_streaks was kept takes r's checks off instead.
func restoreStreaks(b *Babysit, r BabysitRound) {
	if b.prevStreaks != "" {
		prev := map[string]int{}
		if json.Unmarshal([]byte(b.prevStreaks), &prev) == nil {
			b.streaks = prev
			return
		}
	}
	if sameSHA(b.RoundSHA, r.SHA) {
		return
	}
	for _, c := range r.Checks {
		if b.streaks[c]--; b.streaks[c] <= 0 {
			delete(b.streaks, c)
		}
	}
}

// limitLifts is when the owner's limit lifts: as the error said, else the
// reset of a quota window their status line last read as full, else in
// limitRetry, which reports known false.
func (s *Service) limitLifts(ctx context.Context, uid int64, msg string) (time.Time, bool) {
	now := s.now()
	if t, ok := limitResetIn(msg, now); ok && t.Sub(now) <= 8*24*time.Hour {
		return t, true
	}
	var until time.Time
	for _, sp := range quotaSpans {
		l, err := s.currentQuota(ctx, uid, sp.span, now)
		if err != nil {
			s.log.WarnContext(ctx, "read the quota", "user", uid, "err", err)
		}
		if l == nil || l.Last().Pct < 99 {
			continue
		}
		if t := msTime(l.Reset); t.After(now) && t.After(until) {
			until = t
		}
	}
	if until.IsZero() {
		return now.Add(limitRetry), false
	}
	return until, true
}

// limitWait is what the next round waits for while the owner's limit
// holds, or "" once it has lifted.
func (s *Service) limitWait(ctx context.Context, b *Babysit) string {
	if b.limitedUntil <= s.nowMS() || s.limitWasReset(ctx, b) {
		return ""
	}
	at := msTime(b.limitedUntil)
	when := at.Format("15:04 UTC")
	if at.Sub(s.now()) > 20*time.Hour {
		when = at.Format("Jan 2 15:04 UTC")
	}
	return fmt.Sprintf("%s's usage limit stopped the last round, which doesn't count; the next goes after %s", cmp(b.Owner.DisplayName, "its owner"), when)
}

// limitWasReset: the quota window the round waits for was reset since the
// round stopped, and has room again.
func (s *Service) limitWasReset(ctx context.Context, b *Babysit) bool {
	for _, sp := range quotaSpans {
		l, err := s.currentQuota(ctx, b.ownerID, sp.span, s.now())
		if err != nil {
			s.log.WarnContext(ctx, "read the quota", "user", b.ownerID, "err", err)
		}
		if l == nil || l.Last().Pct >= 99 || max(l.Reset-b.limitedUntil, b.limitedUntil-l.Reset) > time.Hour.Milliseconds() {
			continue
		}
		for i := 1; i < len(l.Knots)-1; i++ {
			if l.Knots[i].Zeroed && l.Knots[i+1].At > b.limitedAt {
				return true
			}
		}
	}
	return false
}

// takeBackRound removes round i from b as if it was never handed out: the
// round count and sha go back to the round before it, and the streaks to
// what they were. The caller records it.
func takeBackRound(b *Babysit, i int) BabysitRound {
	r := b.Rounds[i]
	b.Rounds = slices.Delete(b.Rounds, i, i+1)
	b.Round, b.RoundSHA = 0, ""
	if prev := lastRound(b); prev != nil {
		b.Round, b.RoundSHA = prev.N, prev.SHA
	}
	restoreStreaks(b, r)
	return r
}

// withdrawQueuedRound takes back the round of job jobID, the babysit's
// current job, when no runner has the job yet and the head it was for has
// moved on: a push makes it stale, and what the new head needs is decided
// afresh once its checks settle (WP-1618, WP-ADR149). The job is cancelled
// and the babysit recorded without the round in one write, so the job's end
// isn't recorded as the round's outcome and the stored babysit is never
// left with half of it. The wait's clock starts again: the round that
// replaces this one hasn't waited since this one was queued. A job a runner
// has taken is never touched. It reports whether the round was withdrawn; b
// is changed only when it was.
func (s *Service) withdrawQueuedRound(ctx context.Context, b *Babysit, pr *PullRequest, jobID int64) bool {
	var state string
	if err := s.db.R.QueryRowContext(ctx, `SELECT state FROM runner_jobs WHERE id = ?`, jobID).Scan(&state); err != nil || state != "queued" {
		return false
	}
	nb := &Babysit{Rounds: slices.Clone(b.Rounds), Round: b.Round, RoundSHA: b.RoundSHA, streaks: maps.Clone(b.streaks), prevStreaks: b.prevStreaks}
	if i := slices.IndexFunc(nb.Rounds, func(r BabysitRound) bool { return r.Job == jobID }); i >= 0 {
		takeBackRound(nb, i)
	}
	rounds, _ := json.Marshal(nb.Rounds)
	st, _ := json.Marshal(nb.streaks)
	now := s.now()
	cancelled := false
	wctx := context.WithoutCancel(ctx)
	err := s.write(wctx, SystemActor(), func(t *tx) error {
		cancelled = false
		if err := t.QueryRowContext(wctx, `SELECT state FROM runner_jobs WHERE id = ?`, jobID).Scan(&state); err != nil || state != "queued" {
			return err
		}
		res, err := t.ExecContext(wctx, `UPDATE babysits SET job_id = NULL, round = ?, round_sha = ?, rounds = ?, streaks = ?, wait_at = ?, updated_at = ? WHERE id = ? AND job_id = ?`,
			nb.Round, nb.RoundSHA, string(rounds), string(st), now.UnixMilli(), now.UnixMilli(), b.ID, jobID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil
		}
		cancelled = true
		projectID, err := s.attachedProject(wctx, t, pr.ForgeID, pr.Repo)
		if err != nil {
			return err
		}
		t.emit(&Event{Kind: EvBabysit, ProjectID: projectID, Old: b.State, New: b.State, Data: prEventData(pr, map[string]any{"babysit": b.ID, "withdrawn": jobID})})
		return t.finishRunnerJob(wctx, jobID, "cancelled", nil, nil, "Withdrawn: the pull request was pushed to before a runner took the round.")
	})
	if err != nil {
		s.log.WarnContext(ctx, "withdraw a queued babysit round", "pr", pr.ID, "job", jobID, "err", err)
		return false
	}
	if !cancelled {
		return false
	}
	oldHead := b.RoundSHA
	b.Rounds, b.Round, b.RoundSHA, b.streaks, b.JobID, b.WaitSince = nb.Rounds, nb.Round, nb.RoundSHA, nb.streaks, 0, now
	babysitRoundsWithdrawn.Inc()
	s.log.Info("queued babysit round withdrawn: the head moved before a runner took it", "pr", pr.ID, "job", jobID, "old_head", short(oldHead), "new_head", short(pr.HeadSHA))
	s.kickRunners()
	return true
}
