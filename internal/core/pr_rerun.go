package core

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"strings"
	"time"
)

// A caller that goes away doesn't cut a claimed re-run short (WP-1661).
// rerunTimeout bounds the forge's part of it, and rerunRecordTimeout, on a
// clock of its own so a slow forge can't use it up, giving the claim back,
// recording the re-run and reading the checks after.
const (
	rerunTimeout       = 2 * time.Minute
	rerunRecordTimeout = 30 * time.Second
)

// maxManualReruns is how often wp pr rerun re-runs one head's failed jobs
// (WP-1661): a failure that comes back twice is the code's, not a flake.
const maxManualReruns = 2

// PRRerun is what RerunPullRequest asked the forge to run again.
type PRRerun struct {
	Jobs   []string `json:"jobs"`
	Runs   int      `json:"runs,omitempty"`
	RunURL string   `json:"run_url,omitempty"`
	Head   string   `json:"head"`
	// Count is how many times this head has been re-run by the command,
	// of Max.
	Count int `json:"count"`
	Max   int `json:"max"`
	// Partial is why the forge refused the rest, when some jobs went again
	// and then it refused.
	Partial string `json:"partial,omitempty"`
	// Unrecorded: the jobs went again, but Waypoint couldn't write it in the
	// pull request's activity and history.
	Unrecorded bool `json:"unrecorded,omitempty"`
}

// RerunPullRequest re-runs the failed and cancelled jobs of the run that
// counts for each workflow on a pull request's head, or only the job called
// job, without a push (WP-1661). One count covers them all. Whoever may merge
// the pull request may do it, at most twice per head; each is recorded in
// its activity and its history, with what failed.
func (s *Service) RerunPullRequest(ctx context.Context, actor *Actor, prID int64, job string) (*PRRerun, error) {
	pr, err := s.prByID(ctx, s.db.R, prID)
	if err != nil {
		return nil, err
	}
	pid, err := s.mayMergeIn(ctx, actor, pr)
	if err != nil {
		return nil, err
	}
	ref := fmt.Sprintf("%s#%d", pr.Repo, pr.Number)
	if (pr.State != "open" && pr.State != "draft") || pr.HeadSHA == "" {
		return nil, invalid("%s is %s: only an open pull request's head is re-run", ref, pr.State)
	}
	f, err := s.forgeByID(ctx, pr.ForgeID)
	if err != nil {
		return nil, err
	}
	cl, ok := s.Forges.(ForgeJobRerun)
	if !ok || f.token == "" {
		return nil, invalid("this forge connection can't re-run jobs: push a commit or use the forge's Re-run button")
	}
	if can, err := cl.CanRetryChecks(ctx, f); err != nil {
		return nil, fmt.Errorf("ask the forge whether it can re-run jobs: %w", err)
	} else if !can {
		return nil, invalid("%s", strings.TrimPrefix(errForgejoNoRerun.Error(), ErrUnsupported.Error()+": "))
	}
	headID, err := s.trackHead(ctx, pr.ForgeID, pr.Repo, pr.HeadSHA)
	if err != nil {
		return nil, err
	}
	// Claim a re-run first: two callers can't both take the second.
	count, claimed, err := s.claimManualRerun(ctx, headID)
	if err != nil {
		return nil, err
	}
	if !claimed {
		return nil, invalid("%s's head %s was already re-run %d times: push a fix or read why it fails (wp pr log %d)", ref, short(pr.HeadSHA), maxManualReruns, pr.Number)
	}
	// The claim is made: a caller that goes away must not cancel the re-run
	// or leave it unrecorded.
	bg, cancel := context.WithTimeout(context.WithoutCancel(ctx), rerunTimeout)
	defer cancel()
	failedChecks, ferr := s.flakeChecks(bg, pr) // what failed, for the history
	if ferr != nil {
		s.log.WarnContext(ctx, "read the failed checks before a re-run", "pr", pr.ID, "err", ferr)
	}
	s.keepFailedEvidence(bg, f, pr.Repo, pr.HeadSHA) // the forge replaces the failed jobs' logs (WP-704)
	res, err := cl.RerunJobs(bg, f, pr.Repo, pr.HeadSHA, job)
	if res == nil {
		res = &RerunResult{}
	}
	did := len(res.Jobs) + res.Runs
	rec, cancelRec := context.WithTimeout(context.WithoutCancel(ctx), rerunRecordTimeout)
	defer cancelRec()
	if did == 0 { // nothing went again: the claim isn't spent
		if rerr := s.releaseManualRerun(rec, headID); rerr != nil {
			s.log.WarnContext(ctx, "give back a re-run", "pr", pr.ID, "err", rerr)
		}
		count--
	}
	var nm *RerunNoMatch
	switch {
	case errors.As(err, &nm):
		return nil, invalid("no failed job of %s is called %q; the failed jobs are: %s (wp pr rerun %d --job NAME)", short(pr.HeadSHA), nm.Job, strings.Join(nm.Failed, ", "), pr.Number)
	case errors.Is(err, ErrUnsupported) && did == 0:
		return nil, invalid("%s", strings.TrimPrefix(err.Error(), ErrUnsupported.Error()+": "))
	case err != nil && did == 0:
		return nil, fmt.Errorf("re-run the failed jobs of %s: %w", ref, err)
	case err == nil && did == 0 && res.Active > 0:
		return nil, invalid("%s's runs on %s are still going: a failed job can be re-run when its run ends (wp pr wait %d)", ref, short(pr.HeadSHA), pr.Number)
	case err == nil && did == 0:
		return nil, invalid("nothing failed on %s's head %s: wp pr wait %d to see where it stands", ref, short(pr.HeadSHA), pr.Number)
	}
	partial := ""
	if err != nil { // some went again before the forge refused
		partial = excerpt(err.Error(), 200)
		s.log.WarnContext(ctx, "re-run failed jobs: the forge refused part", "pr", pr.ID, "err", err)
	}
	what := fmt.Sprintf("%d run%s", res.Runs, plural(res.Runs))
	if len(res.Jobs) > 0 {
		what = strings.Join(res.Jobs, ", ")
	}
	detail := fmt.Sprintf("re-ran %s on %s (%d of %d for this head)", excerpt(what, 200), short(pr.HeadSHA), count, maxManualReruns)
	if partial != "" {
		detail += "; the forge refused the rest: " + partial
	}
	// The ext id is this re-run's own: a count given back is claimed again.
	extID := fmt.Sprintf("rerun:%s:%s", pr.HeadSHA, rand.Text())
	werr := s.write(rec, actor, func(t *tx) error {
		t.audit(AuditEntry{Action: "pr.rerun", Category: AuditControl, ProjectID: pid, TargetType: "pr", TargetID: pr.ID,
			Target: ref, URL: pr.URL, Detail: detail, About: pr.Agent})
		t.emit(&Event{Kind: EvPRActivity, ProjectID: pid, New: "rerun", Data: map[string]any{"pr_id": pr.ID, "head_sha": pr.HeadSHA}})
		return s.savePREvents(rec, t, pr, []prEventRow{{kind: "ci_rerun", actor: actor.User.Username, sha: pr.HeadSHA, url: res.RunURL,
			body: rerunHistory(detail, failedChecks), extID: extID, at: s.nowMS()}}, pid, false)
	})
	if werr != nil { // the jobs went: not an error to the caller, who is told
		s.log.WarnContext(ctx, "record a re-run", "pr", pr.ID, "detail", detail, "err", werr)
	}
	s.refreshPRChecks(rec, pr.ID) // the checks show pending at once
	return &PRRerun{Jobs: res.Jobs, Runs: res.Runs, RunURL: res.RunURL, Head: pr.HeadSHA, Count: count, Max: maxManualReruns, Partial: partial, Unrecorded: werr != nil}, nil
}

// rerunHistory is a re-run's line in the pull request's history: what went
// again, then what each failed check had failed, by its failing tests or the
// last lines of its log.
func rerunHistory(detail string, checks []flakeCheck) string {
	var b strings.Builder
	b.WriteString(detail)
	for _, c := range checks {
		what := strings.Join(c.Tests, ", ")
		if what == "" {
			lines := strings.Split(strings.TrimSpace(c.Excerpt), "\n")
			what = strings.Join(lines[max(len(lines)-2, 0):], " / ")
		}
		b.WriteString("\n" + c.Name)
		if what = strings.TrimSpace(what); what != "" {
			b.WriteString(": " + excerpt(what, 150))
		}
	}
	return b.String()
}

// claimManualRerun takes one of a head's re-runs: its new count, and false
// when it has had them all.
func (s *Service) claimManualRerun(ctx context.Context, headID int64) (int, bool, error) {
	count, claimed := 0, false
	err := s.write(ctx, SystemActor(), func(t *tx) error {
		res, err := t.ExecContext(ctx, `UPDATE ci_heads SET manual_reruns = manual_reruns + 1 WHERE id = ? AND manual_reruns < ?`, headID, maxManualReruns)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		claimed = n == 1
		return t.QueryRowContext(ctx, `SELECT manual_reruns FROM ci_heads WHERE id = ?`, headID).Scan(&count)
	})
	return count, claimed, err
}

func (s *Service) releaseManualRerun(ctx context.Context, headID int64) error {
	return s.write(ctx, SystemActor(), func(t *tx) error {
		_, err := t.ExecContext(ctx, `UPDATE ci_heads SET manual_reruns = MAX(manual_reruns - 1, 0) WHERE id = ?`, headID)
		return err
	})
}
