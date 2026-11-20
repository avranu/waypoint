package core

// A forge still checking (WP-138). Forgejo and Gitea answer mergeable=false
// while they work out whether a pull request merges: after a push, after its
// base moved, or while their patch-checker queue is backed up. That "no" is
// not a conflict, and a merge tried meanwhile is refused with "Please try
// again later". So a "no" is only believed once a read some time after the
// head or base changed repeats it (or a long time has passed); until then
// it is "still checking": readiness says so, `wp pr wait` keeps waiting,
// babysitting sends nobody to fix it. Pull requests someone depends on
// (armed, babysat, waited on) are read again soon instead of at the next
// sync, and an armed merge the forge refused as "try again later" is tried
// again with backoff. A pull request titled work in progress gets "no"
// whatever its state, so its answer is not recorded at all (WP-171).

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"
)

const (
	// mergeConfirmAfter: a "no" read at least this long after the head or
	// base changed is believed.
	mergeConfirmAfter = 90 * time.Second
	// mergeCheckingLimit: after this long a "no" is believed whatever was read.
	mergeCheckingLimit = 15 * time.Minute
)

// mergeRecheckDelays: how long after the last read (or the first sight of
// the "no") Waypoint reads the pull request again.
var mergeRecheckDelays = []time.Duration{30 * time.Second, 2 * time.Minute, 5 * time.Minute}

// mergeRetryDelays: how long after a refusal of "try again later" an armed
// merge is tried again; after these, every mergeRetryThen (the sync).
var mergeRetryDelays = []time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute}

const mergeRetryThen = 10 * time.Minute

// mergeState reads the forge's answer on whether the pull request merges:
// conflict when a "no" is believed, checking when there is a "no" that is
// not yet.
func (p *PullRequest) mergeState(now time.Time) (conflict, checking bool) {
	if p.Mergeable == nil || *p.Mergeable {
		return false, false
	}
	var since time.Time
	for _, t := range []*time.Time{p.headAt, p.mergeSince} {
		if t != nil && t.After(since) {
			since = *t
		}
	}
	switch {
	case since.IsZero(): // recorded before Waypoint kept track
		return true, false
	case now.Sub(since) >= mergeCheckingLimit:
		return true, false
	case p.mergeReadAt != nil && p.mergeReadAt.Sub(since) >= mergeConfirmAfter:
		return true, false
	}
	return false, true
}

// noteMergeable stores the forge's answer on whether the pull request
// merges, read now. prev is the pull request as recorded before this read
// (its head already the new one); headChanged says the head is new.
func (s *Service) noteMergeable(ctx context.Context, x queryer, prev *PullRequest, headChanged bool, read *bool) error {
	now := s.nowMS()
	switch {
	case read != nil && *read:
		_, err := x.ExecContext(ctx, `UPDATE pull_requests SET mergeable = 1, mergeable_read_at = ?, mergeable_since = NULL, recheck_at = NULL, recheck_n = 0 WHERE id = ?`, now, prev.ID)
		return err
	case read != nil:
		// A new "no": the first read, or after a yes, or for a new head.
		fresh := headChanged || prev.Mergeable == nil || *prev.Mergeable
		var since any
		if prev.mergeSince != nil {
			since = prev.mergeSince.UnixMilli()
		}
		if fresh {
			since = now
		}
		_, err := x.ExecContext(ctx, `UPDATE pull_requests SET mergeable = 0, mergeable_read_at = ?, mergeable_since = ?,
			recheck_at = CASE WHEN ? THEN NULL ELSE recheck_at END, recheck_n = CASE WHEN ? THEN 0 ELSE recheck_n END WHERE id = ?`, now, since, fresh, fresh, prev.ID)
		return err
	case headChanged:
		// Nothing read for the new head: the old answer is no longer news.
		_, err := x.ExecContext(ctx, `UPDATE pull_requests SET mergeable_since = ?, recheck_at = NULL, recheck_n = 0 WHERE id = ?`, now, prev.ID)
		return err
	}
	return nil
}

// forgetMergeable drops the recorded answer: the forge's now says nothing
// (a work-in-progress title, WP-171).
func forgetMergeable(ctx context.Context, x queryer, prID int64) error {
	_, err := x.ExecContext(ctx, `UPDATE pull_requests SET mergeable = NULL, mergeable_read_at = NULL, mergeable_since = NULL, recheck_at = NULL, recheck_n = 0 WHERE id = ?`, prID)
	return err
}

// forgeChecking: the forge isn't done working out whether the pull request
// merges. It is a validation error for callers, and what an armed merge
// retries on.
type forgeChecking struct{ *InvalidError }

func (e *forgeChecking) Unwrap() error { return e.InvalidError }

func checkingErr(msg string) error { return &forgeChecking{&InvalidError{Msg: msg}} }

// transientMergeRefusal: the forge refused a merge it would accept shortly:
// Forgejo and Gitea's "Please try again later" while a pull request is
// being checked, GitHub's "Base branch was modified".
func transientMergeRefusal(err error) bool {
	var ae *forgeAPIError
	if !errors.As(err, &ae) {
		return false
	}
	body := strings.ToLower(ae.body)
	if strings.Contains(body, "try again later") {
		return true
	}
	return ae.status == http.StatusMethodNotAllowed && strings.Contains(body, "base branch was modified")
}

// wantsMergeable: someone depends on the forge's answer about this pull
// request: it is armed, babysat, or a wait is held on it.
func (s *Service) wantsMergeable(ctx context.Context, pr *PullRequest) bool {
	if pr.ArmedAt != nil || s.prWait.waiting(pr.ID) {
		return true
	}
	var n int
	return s.db.R.QueryRowContext(ctx, `SELECT count(*) FROM babysits WHERE pr_id = ? AND ended_at IS NULL`, pr.ID).Scan(&n) == nil && n > 0
}

// scheduleMergeRecheck has the pull request read from the forge again soon
// if its "no" is unconfirmed and someone depends on it.
func (s *Service) scheduleMergeRecheck(ctx context.Context, prID int64) {
	pr, err := s.prByID(ctx, s.db.R, prID)
	if err != nil || (pr.State != "open" && pr.State != "draft") {
		return
	}
	if _, checking := pr.mergeState(s.now()); !checking || !s.wantsMergeable(ctx, pr) {
		return
	}
	var n int
	var at *int64
	if err := s.db.R.QueryRowContext(ctx, `SELECT recheck_n, recheck_at FROM pull_requests WHERE id = ?`, prID).Scan(&n, &at); err != nil || at != nil || n >= len(mergeRecheckDelays) {
		return
	}
	if _, err := s.db.W.ExecContext(ctx, `UPDATE pull_requests SET recheck_at = ? WHERE id = ? AND recheck_at IS NULL`, s.now().Add(mergeRecheckDelays[n]).UnixMilli(), prID); err != nil {
		s.log.WarnContext(ctx, "best-effort write failed", "err", err)
	}
}

// CheckMergeRechecks reads the pull requests whose time has come from their
// forge (WP-138), runs what the answer decides, and schedules the next read
// while the forge still says no.
func (s *Service) CheckMergeRechecks(ctx context.Context) {
	rows, err := s.db.R.QueryContext(ctx, `SELECT id FROM pull_requests WHERE recheck_at IS NOT NULL AND recheck_at <= ? AND state IN ('open','draft') ORDER BY recheck_at LIMIT 100`, s.nowMS())
	if err != nil {
		return
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	for _, id := range ids {
		if ctx.Err() != nil {
			return
		}
		s.recheckMergeable(ctx, id)
	}
}

func (s *Service) recheckMergeable(ctx context.Context, prID int64) {
	if _, err := s.db.W.ExecContext(ctx, `UPDATE pull_requests SET recheck_at = NULL, recheck_n = recheck_n + 1 WHERE id = ?`, prID); err != nil {
		return
	}
	pr, err := s.prByID(ctx, s.db.R, prID)
	if err != nil {
		return
	}
	f, err := s.forgeByID(ctx, pr.ForgeID)
	prc, ok := s.Forges.(ForgePRs)
	if err != nil || !ok || f.token == "" {
		return
	}
	fp, err := prc.GetPR(ctx, f, pr.Repo, pr.Number)
	if err != nil {
		s.log.Debug("recheck: read the pull request", "repo", pr.Repo, "number", pr.Number, "err", err)
		s.scheduleMergeRecheck(ctx, prID)
		return
	}
	cur, err := s.recordPR(ctx, f, pr.Repo, fp, false)
	if err != nil || cur == nil {
		return
	}
	s.scheduleMergeRecheck(ctx, prID)
	if cur.ArmedAt != nil {
		s.maybeAutoMerge(ctx, prID)
	}
	s.nudgeBabysit(prID, false)
	s.prWait.wake(&Event{Data: map[string]any{"pr_id": prID}})
}

// retryArmedMerge notes that the forge refused an armed merge as "try again
// later" and has it tried again with backoff: after a minute, five, fifteen,
// then every sync, for this head.
func (s *Service) retryArmedMerge(ctx context.Context, pr *PullRequest, why string) {
	n := 0
	if pr.mergeRetrySHA == pr.HeadSHA {
		n = pr.mergeRetryN
	}
	delay := mergeRetryThen
	if n < len(mergeRetryDelays) {
		delay = mergeRetryDelays[n]
	}
	at := s.now().Add(delay)
	if _, err := s.db.W.ExecContext(ctx, `UPDATE pull_requests SET merge_retry_n = ?, merge_retry_sha = ? WHERE id = ?`, n+1, pr.HeadSHA, pr.ID); err != nil {
		s.log.WarnContext(ctx, "best-effort write failed", "err", err)
	}
	s.armNote(ctx, pr.ID, why+"; Waypoint will try again at "+at.UTC().Format("15:04")+" UTC")
	s.setMergeWake(ctx, pr.ID, &at)
}
