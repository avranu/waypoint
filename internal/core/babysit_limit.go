package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"git.jmann.me/claude/waypoint/internal/lazyre"
)

// A round whose agent stopped at its owner's usage limit did no work on
// the pull request, so it isn't one of its rounds (WP-683): counted, it
// took the next rung, and an assessor round that hit the limit parked the
// pull request for a person. It is dropped, and the next round waits until
// the limit lifts, out of the merge line meanwhile.

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
	r := b.Rounds[i]
	b.Rounds = slices.Delete(b.Rounds, i, i+1)
	b.Round, b.RoundSHA = 0, ""
	if prev := lastRound(b); prev != nil {
		b.Round, b.RoundSHA = prev.N, prev.SHA
	}
	restoreStreaks(b, r)
	b.limitedUntil, b.limitTries, b.JobID = until.UnixMilli(), tries, 0
	rounds, _ := json.Marshal(b.Rounds)
	st, _ := json.Marshal(b.streaks)
	if _, err := s.db.W.ExecContext(ctx, `UPDATE babysits SET job_id = NULL, round = ?, round_sha = ?, rounds = ?, streaks = ?, limited_until = ?, limited_tries = ?, updated_at = ? WHERE id = ?`,
		b.Round, b.RoundSHA, string(rounds), string(st), b.limitedUntil, b.limitTries, s.nowMS(), b.ID); err != nil {
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
		return // a round on the same head again didn't add to them
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
		var pct float64
		var reset int64
		err := s.db.R.QueryRowContext(ctx, `SELECT percent, resets_at FROM quota_samples WHERE user_id = ? AND span = ? ORDER BY at DESC LIMIT 1`, uid, sp.span).Scan(&pct, &reset)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			s.log.WarnContext(ctx, "read the quota", "user", uid, "err", err)
		}
		if t := msTime(reset); err == nil && pct >= 99 && t.After(now) && t.After(until) {
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
func (s *Service) limitWait(b *Babysit) string {
	if b.limitedUntil <= s.nowMS() {
		return ""
	}
	at := msTime(b.limitedUntil)
	when := at.Format("15:04 UTC")
	if at.Sub(s.now()) > 20*time.Hour {
		when = at.Format("Jan 2 15:04 UTC")
	}
	return fmt.Sprintf("%s's usage limit stopped the last round, which doesn't count; the next goes after %s", cmp(b.Owner.DisplayName, "its owner"), when)
}
