package core

import (
	"context"
	"errors"
	"fmt"
	"strings"
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
}

// RerunPullRequest re-runs the failed and cancelled jobs of the newest run
// on a pull request's head, or only the job called job, without a push
// (WP-1661). Whoever may merge the pull request may do it, at most twice per
// head; each is recorded in its activity.
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
	bg := context.WithoutCancel(ctx)
	s.keepFailedEvidence(bg, f, pr.Repo, pr.HeadSHA) // the re-run replaces their logs (WP-704)
	res, err := cl.RerunJobs(ctx, f, pr.Repo, pr.HeadSHA, job)
	if res == nil {
		res = &RerunResult{}
	}
	did := len(res.Jobs) + res.Runs
	if did == 0 { // nothing went again: the claim isn't spent
		if rerr := s.releaseManualRerun(bg, headID); rerr != nil {
			s.log.WarnContext(ctx, "give back a re-run", "pr", pr.ID, "err", rerr)
		}
		count--
	}
	var nm *RerunNoMatch
	switch {
	case errors.As(err, &nm):
		return nil, invalid("no failed job of %s is called %q; the failed jobs are: %s (wp pr rerun %d --job NAME)", short(pr.HeadSHA), nm.Job, strings.Join(nm.Failed, ", "), pr.Number)
	case errors.Is(err, ErrUnsupported):
		return nil, invalid("%s", strings.TrimPrefix(err.Error(), ErrUnsupported.Error()+": "))
	case err != nil && did == 0:
		return nil, fmt.Errorf("re-run the failed jobs of %s: %w", ref, err)
	case err == nil && did == 0:
		return nil, invalid("nothing failed on %s's head %s: wp pr wait %d to see where it stands", ref, short(pr.HeadSHA), pr.Number)
	}
	if err != nil { // some went again before the forge refused
		s.log.WarnContext(ctx, "re-run failed jobs: the forge refused part", "pr", pr.ID, "err", err)
	}
	what := fmt.Sprintf("%d run%s", res.Runs, plural(res.Runs))
	if len(res.Jobs) > 0 {
		what = strings.Join(res.Jobs, ", ")
	}
	if werr := s.write(ctx, actor, func(t *tx) error {
		t.audit(AuditEntry{Action: "pr.rerun", Category: AuditControl, ProjectID: pid, TargetType: "pr", TargetID: pr.ID,
			Target: ref, URL: pr.URL, Detail: fmt.Sprintf("re-ran %s on %s (%d of %d for this head)", excerpt(what, 200), short(pr.HeadSHA), count, maxManualReruns), About: pr.Agent})
		t.emit(&Event{Kind: EvPRActivity, ProjectID: pid, New: "rerun", Data: map[string]any{"pr_id": pr.ID, "head_sha": pr.HeadSHA}})
		return nil
	}); werr != nil {
		s.log.WarnContext(ctx, "record a re-run", "pr", pr.ID, "err", werr)
	}
	s.refreshPRChecks(bg, pr.ID) // the checks show pending at once
	return &PRRerun{Jobs: res.Jobs, Runs: res.Runs, RunURL: res.RunURL, Head: pr.HeadSHA, Count: count, Max: maxManualReruns}, nil
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
