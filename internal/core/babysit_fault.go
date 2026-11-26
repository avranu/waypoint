package core

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"git.jmann.me/claude/waypoint/internal/lazyre"
)

// A round whose job ended before its agent started did no work and says
// nothing about the pull request: a clone that couldn't sign in, a runner
// lost, a checkout that timed out. Counting it spent the cap and, on the same
// head, sent the retry to a stronger model for items nobody had looked at
// (WP-1914, 20 of 237 rounds in #520 to #618). It is taken back as a usage
// limit's drop is (babysit_limit.go) and tried again on the same rung.

const (
	faultWaitFirst = 2 * time.Minute
	faultWaitMax   = 30 * time.Minute
	// faultsToTell is how many faults in a row on one babysit reach the admins.
	faultsToTell = 3
)

// faultTexts are what the runner said before it set a fault in its result
// (runners that haven't upgraded), and what Waypoint says for a job it ended.
var faultTexts = []string{"can't get the repository:", "isn't signed in on this machine", "isn't installed on this machine", "while preparing the checkout.", "No runner took this job within a day."}

// roundFaultLost is the fault of a job the runner lost or that was stopped: its
// agent may have run, and a round that itself kills the runner would go
// again for ever, so only the first faultsToTell in a row go uncounted.
const roundFaultLost = "lost"

// roundFault is the fault that ended the round's job before its agent
// started, or "". An agent's own error and a work timeout aren't faults.
func roundFault(job *RunnerJob) string {
	if job.State == "done" {
		return ""
	}
	var res struct {
		Fault string `json:"fault"`
	}
	if len(job.Result) > 0 && json.Unmarshal(job.Result, &res) == nil && res.Fault != "" {
		return res.Fault
	}
	if job.State == "failed" && agentFailure.MatchString(job.Error) {
		return ""
	}
	if slices.ContainsFunc(faultTexts, func(t string) bool { return strings.Contains(job.Error, t) }) {
		return "runner"
	}
	if strings.Contains(job.Error, "The runner lost this job") || strings.Contains(job.Error, runnerStoppedError) {
		return roundFaultLost
	}
	return ""
}

// roundFaultBackoff is the wait after the nth fault in a row.
func roundFaultBackoff(n int) time.Duration {
	d := faultWaitFirst
	for ; n > 1 && d < faultWaitMax; n-- {
		d *= 2
	}
	return min(d, faultWaitMax)
}

// uncountFaultedRound drops the round of job jobID, which ended in a fault,
// from the babysit and sets when the next may go. After faultsToTell in a row
// it tells the admins, once; it keeps going at the longest wait.
func (s *Service) uncountFaultedRound(ctx context.Context, b *Babysit, jobID int64, msg, kind string) bool {
	i := slices.IndexFunc(b.Rounds, func(r BabysitRound) bool { return r.Job == jobID })
	if i < 0 {
		return false
	}
	var tries int
	_ = s.db.R.QueryRowContext(ctx, `SELECT fault_tries FROM babysits WHERE id = ?`, b.ID).Scan(&tries)
	if kind == roundFaultLost && tries >= faultsToTell {
		return false
	}
	tries++
	r := takeBackRound(b, i)
	until := s.now().Add(roundFaultBackoff(tries))
	b.limitedUntil, b.JobID = until.UnixMilli(), 0
	rounds, _ := json.Marshal(b.Rounds)
	st, _ := json.Marshal(b.streaks)
	if _, err := s.db.W.ExecContext(ctx, `UPDATE babysits SET job_id = NULL, round = ?, round_sha = ?, rounds = ?, streaks = ?, limited_until = ?, fault_tries = ?, fault_text = ?, updated_at = ? WHERE id = ?`,
		b.Round, b.RoundSHA, string(rounds), string(st), b.limitedUntil, tries, excerpt(msg, 300), s.nowMS(), b.ID); err != nil {
		s.log.WarnContext(ctx, "record a babysit round", "babysit", b.ID, "err", err)
		return true
	}
	s.log.Info("babysit round ended before its agent started; not counted", "babysit", b.ID, "round", r.N, "job", jobID, "faults", tries, "until", until, "err", excerpt(msg, 200))
	if tries == faultsToTell {
		s.tellFaultedRounds(ctx, b, tries, msg)
	}
	return true
}

// tellFaultedRounds tells the admins a babysit's rounds keep failing before
// their agent starts: the owner's runner or its checkout is what to fix.
func (s *Service) tellFaultedRounds(ctx context.Context, b *Babysit, tries int, msg string) {
	var repo string
	var number int
	_ = s.db.R.QueryRowContext(ctx, `SELECT repo, number FROM pull_requests WHERE id = ?`, b.PRID).Scan(&repo, &number)
	title := fmt.Sprintf("%s#%d: %d babysit rounds in a row failed before their agent started", repo, number, tries)
	body := fmt.Sprintf("%s. They aren't counted and the next goes at most %s after the last, but they don't stop on their own: look at the runner (wp server status, wp server log --level warn).", excerpt(msg, 300), ShortDuration(faultWaitMax))
	for _, id := range s.userIDsWhere(ctx, `SELECT id FROM users WHERE is_admin = 1 AND disabled = 0 ORDER BY id`) {
		s.notifyURL(ctx, id, nil, NotifyJobs, title, body, "")
	}
}

// faultWait is what the next round waits for after a round ended in a fault,
// or "" once the wait is over.
func (s *Service) faultWait(ctx context.Context, b *Babysit) string {
	if b.limitedUntil <= s.nowMS() {
		return ""
	}
	var text string
	var tries int
	if err := s.db.R.QueryRowContext(ctx, `SELECT fault_text, fault_tries FROM babysits WHERE id = ?`, b.ID).Scan(&text, &tries); err != nil || tries == 0 {
		return ""
	}
	return fmt.Sprintf("the last round didn't start, so it isn't counted (%s); the next goes after %s", text, msTime(b.limitedUntil).Format("15:04 UTC"))
}

// pushRefused starts the error of a round whose commits the runner couldn't
// push (cmd/wp/runner_agent.go).
var pushRefused = lazyre.New(`^pushing \S+ failed: `)

// lastPushFailed: the last round's push was refused, so the head it left
// is not evidence that the model's work failed to fix anything.
func lastPushFailed(b *Babysit) bool {
	r := lastRound(b)
	return r != nil && r.PushFailed
}
