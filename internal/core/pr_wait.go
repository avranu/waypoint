package core

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"git.jmann.me/claude/waypoint/internal/lazyre"
)

// Wait verdicts.
const (
	PRWaitReady    = "ready"
	PRWaitMerged   = "merged"
	PRWaitClosed   = "closed"
	PRWaitFailed   = "failed"
	PRWaitConflict = "conflict"
	PRWaitBlocked  = "blocked"
	PRWaitWaiting  = "waiting"
)

const (
	prWaitMax     = 5 * time.Minute
	prWaitRecheck = time.Minute
)

// PRWait is where a pull request stands when a wait ends.
type PRWait struct {
	Verdict string `json:"verdict"`
	// Why says it in a few words: the failed checks, the conflict, what
	// blocks it, or what it is still waiting for.
	Why       string      `json:"why,omitempty"`
	ID        int64       `json:"id"`
	Repo      string      `json:"repo"`
	Number    int         `json:"number"`
	URL       string      `json:"url"`
	State     string      `json:"state"`
	BaseRef   string      `json:"base_ref"`
	HeadSHA   string      `json:"head_sha"`
	MergedSHA string      `json:"merged_sha,omitempty"`
	CI        string      `json:"ci"`
	Checks    []PRCheck   `json:"checks"`
	Readiness PRReadiness `json:"readiness"`
	Armed     bool        `json:"armed"`
	ArmedNote string      `json:"armed_note,omitempty"`
}

// PRWaitInput says how long to wait, and optionally for which head.
type PRWaitInput struct {
	Timeout time.Duration // 0: say where it stands now; at most five minutes
	// Head, when set, is the commit the caller pushed: until Waypoint sees
	// it as the pull request's head, the older head's checks don't count.
	Head string
}

// prWaiters wakes held waits when an event about their pull request is
// dispatched.
type prWaiters struct {
	mu   sync.Mutex
	m    map[int64]map[chan struct{}]bool
	read map[int64]time.Time
}

// prWaitReadEvery is the least time between two reads of one pull request
// from the forge for waits, however many wait or ask again.
const prWaitReadEvery = 20 * time.Second

// mayRead says whether a wait may read the pull request from the forge now,
// and if so notes that it does.
func (w *prWaiters) mayRead(prID int64) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.read == nil {
		w.read = map[int64]time.Time{}
	}
	if time.Since(w.read[prID]) < prWaitReadEvery {
		return false
	}
	for id, at := range w.read {
		if time.Since(at) > prWaitReadEvery {
			delete(w.read, id)
		}
	}
	w.read[prID] = time.Now()
	return true
}

// waiting says whether a wait is held on the pull request.
func (w *prWaiters) waiting(prID int64) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.m[prID]) > 0
}

func (w *prWaiters) add(prID int64) (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	w.mu.Lock()
	if w.m == nil {
		w.m = map[int64]map[chan struct{}]bool{}
	}
	if w.m[prID] == nil {
		w.m[prID] = map[chan struct{}]bool{}
	}
	w.m[prID][ch] = true
	w.mu.Unlock()
	return ch, func() {
		w.mu.Lock()
		delete(w.m[prID], ch)
		if len(w.m[prID]) == 0 {
			delete(w.m, prID)
		}
		w.mu.Unlock()
	}
}

// wake nudges the waits on the pull request an event is about.
func (w *prWaiters) wake(e *Event) {
	var id int64
	for _, k := range []string{"pr_id", "pr"} {
		if v, ok := e.Data[k].(int64); ok {
			id = v
			break
		}
	}
	if id == 0 {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	for ch := range w.m[id] {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// WaitPullRequest holds until the pull request settles (see PRWait's
// verdicts) or the time is up, and says where it stands.
func (s *Service) WaitPullRequest(ctx context.Context, actor *Actor, prID int64, in PRWaitInput) (*PRWait, error) {
	pr, err := s.prByID(ctx, s.db.R, prID)
	if err != nil {
		return nil, err
	}
	if _, err := s.prAuthority(ctx, actor, pr, RoleViewer); err != nil {
		return nil, err
	}
	in.Timeout = min(max(in.Timeout, 0), prWaitMax)
	woken, done := s.prWait.add(prID)
	defer done()
	s.scheduleMergeRecheck(ctx, prID)
	if in.Head != "" && !sameSHA(pr.HeadSHA, in.Head) {
		s.prWaitRefresh(ctx, pr)
	}
	deadline := time.NewTimer(in.Timeout)
	defer deadline.Stop()
	recheck := time.NewTicker(prWaitRecheck)
	defer recheck.Stop()
	for {
		w, err := s.prWaitState(ctx, prID, in.Head)
		if err != nil || w.Verdict != PRWaitWaiting {
			return w, err
		}
		select {
		case <-woken:
		case <-recheck.C:
			if pr, err := s.prByID(ctx, s.db.R, prID); err == nil {
				s.prWaitRefresh(ctx, pr)
			}
		case <-deadline.C:
			return w, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// sameSHA: one is the other, or its abbreviation.
func sameSHA(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	a, b = strings.ToLower(a), strings.ToLower(b)
	return strings.HasPrefix(a, b) || strings.HasPrefix(b, a)
}

// prWaitRefresh reads the pull request from the forge for a wait, at most
// every prWaitReadEvery.
func (s *Service) prWaitRefresh(ctx context.Context, pr *PullRequest) {
	s.prWaitRead(ctx, pr, false)
}

// armedReconcileEvery and armedReconcileMax bound CheckArmedHeads.
const (
	armedReconcileEvery = 10 * time.Minute
	armedReconcileMax   = 30
)

// CheckArmedHeads reads armed open pull requests that nothing has refreshed
// for ten minutes from the forge, oldest first: a missed push webhook would
// otherwise leave one on an old head for hours (WP-163).
func (s *Service) CheckArmedHeads(ctx context.Context) {
	rows, err := s.db.R.QueryContext(ctx, `SELECT id FROM pull_requests WHERE state = 'open' AND armed_at IS NOT NULL AND updated_at < ?
		ORDER BY updated_at LIMIT ?`, s.nowMS()-armedReconcileEvery.Milliseconds(), armedReconcileMax)
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
		if pr, err := s.prByID(ctx, s.db.R, id); err == nil {
			s.prWaitRead(ctx, pr, true)
		}
	}
}

// prWaitRead is prWaitRefresh; force skips the rate limit. It returns the
// head the forge says the pull request has now (empty when it wasn't read).
func (s *Service) prWaitRead(ctx context.Context, pr *PullRequest, force bool) string {
	ctx = context.WithoutCancel(ctx)
	f, err := s.forgeByID(ctx, pr.ForgeID)
	prc, ok := s.Forges.(ForgePRs)
	if err != nil || !ok || f.token == "" || (!force && !s.prWait.mayRead(pr.ID)) {
		return ""
	}
	fp, err := prc.GetPR(ctx, f, pr.Repo, pr.Number)
	if err != nil {
		s.log.Debug("wait: read the pull request", "repo", pr.Repo, "number", pr.Number, "err", err)
		return ""
	}
	cur, err := s.recordPR(ctx, f, pr.Repo, fp, false)
	if err != nil || cur == nil {
		return fp.HeadSHA
	}
	switch {
	case cur.HeadSHA != pr.HeadSHA || cur.ciSHA != cur.HeadSHA:
		s.afterPRRecorded(ctx, f, cur)
	case cur.ArmedAt != nil:
		s.goBG(func() { s.maybeAutoMerge(ctx, cur.ID) })
	}
	s.prWait.wake(&Event{Data: map[string]any{"pr_id": pr.ID}})
	return fp.HeadSHA
}

// headSuperseded says whether the forge already has a newer head than the
// one Waypoint recorded (a push whose webhook hasn't come). The forge
// cancels the old head's run then, and its cancelled checks look like
// failures (WP-153). It records the new head, which follows its checks.
func (s *Service) headSuperseded(ctx context.Context, pr *PullRequest) (string, bool) {
	head := s.prWaitRead(ctx, pr, true)
	if head != "" && !sameSHA(head, pr.HeadSHA) {
		return head, true
	}
	return "", false
}

// prWithChecks reads a pull request and its head's checks as one.
// refreshHead writes both in one transaction; a read that straddles it
// would pair the old CI state with the new checks, a failure with none
// running that the wait still calls pending.
func (s *Service) prWithChecks(ctx context.Context, prID int64) (*PullRequest, []PRCheck, error) {
	var pr *PullRequest
	var checks []PRCheck
	for range 3 {
		var err error
		if pr, err = s.prByID(ctx, s.db.R, prID); err != nil {
			return nil, nil, err
		}
		if checks, err = s.prChecks(ctx, pr); err != nil {
			return nil, nil, err
		}
		again, err := s.prByID(ctx, s.db.R, prID)
		if err != nil {
			return nil, nil, err
		}
		if again.HeadSHA == pr.HeadSHA && again.CIState == pr.CIState && again.ciSHA == pr.ciSHA {
			break
		}
	}
	return pr, checks, nil
}

// prWaitState decides whether the pull request has settled.
func (s *Service) prWaitState(ctx context.Context, prID int64, head string) (*PRWait, error) {
	pr, checks, err := s.prWithChecks(ctx, prID)
	if err != nil {
		return nil, err
	}
	w := &PRWait{Verdict: PRWaitWaiting, ID: pr.ID, Repo: pr.Repo, Number: pr.Number, URL: pr.URL, State: pr.State,
		BaseRef: pr.BaseRef, HeadSHA: pr.HeadSHA, CI: pr.CIState, Checks: checks, Armed: pr.ArmedAt != nil, ArmedNote: pr.armedNote}
	if pr.ciSHA != pr.HeadSHA {
		w.CI = ""
	}
	rd, err := s.readiness(ctx, pr)
	if err != nil {
		return nil, err
	}
	w.Readiness = *rd
	settle := func(verdict, why string) (*PRWait, error) {
		w.Verdict, w.Why = verdict, why
		return w, nil
	}
	switch pr.State {
	case "merged":
		w.MergedSHA = s.mergedSHA(ctx, pr)
		return settle(PRWaitMerged, "")
	case "closed":
		return settle(PRWaitClosed, "closed without merging")
	}
	conflict, _ := pr.mergeState(s.now())
	switch {
	case pr.HeadSHA == "":
		w.Why = "its head isn't known yet"
		return w, nil
	case head != "" && !sameSHA(pr.HeadSHA, head):
		w.Why = "Waypoint hasn't seen " + short(head) + " on it yet"
		return w, nil
	case rd.Checking:
		w.Why = "the forge is still checking whether it merges with " + cmp(pr.BaseRef, "its base")
		return w, nil
	case conflict:
		return settle(PRWaitConflict, reasonConflict(pr.BaseRef))
	case w.CI == "":
		w.Why = "checks haven't been read yet"
		return w, nil
	case w.CI == CIFailed || (w.CI == CIPending && failedNotCancelled(w.Checks)):
		if moved, ok := s.headSuperseded(ctx, pr); ok {
			w.Why = "a newer head, " + short(moved) + ", is being read"
			return w, nil
		}
		_, why := ciSummary(CIFailed, w.Checks)
		if faults := s.headRunnerFaults(ctx, pr); faults != nil {
			why += " (" + faultSummary(faults) + "); " + faultRetryWords(faults)
		}
		if w.CI == CIPending {
			n := 0
			for _, c := range w.Checks {
				if c.State == CheckPending {
					n++
				}
			}
			why += fmt.Sprintf(" (%d still running)", n)
		}
		return settle(PRWaitFailed, why)
	case w.CI == CIPending:
		_, w.Why = ciSummary(CIPending, w.Checks)
		return w, nil
	}
	if rd.Ready {
		if w.Armed && w.ArmedNote == "" {
			w.Why = "ready; merge when ready is merging it"
			return w, nil
		}
		if w.Armed && pr.mergeRetryN > 0 && pr.mergeRetrySHA == pr.HeadSHA {
			w.Why = w.ArmedNote
			return w, nil
		}
		return settle(PRWaitReady, "")
	}
	if going, why := s.reviewGoing(ctx, pr); going {
		w.Why = why
		return w, nil
	}
	return settle(PRWaitBlocked, strings.Join(rd.Reasons, "; "))
}

// reviewGoing says whether a review of the head is still to come: the gate
// hasn't been evaluated on it, a review bot is running or queued, or an audit
// of the pull request is running (WP-355). A queued
// one holds only within the repository's review wait, when it has one: a
// review parked on a model's quota or behind a backlog of others would
// otherwise hold the pull request, and the merge queue behind it, for hours.
// A review that lands later is still read on the next sweep.
func (s *Service) reviewGoing(ctx context.Context, pr *PullRequest) (bool, string) {
	going, why, rerr := s.reviewGoingErr(ctx, pr)
	if rerr != nil {
		if readFault(rerr) {
			s.log.WarnContext(ctx, "review going err failed", "repo", pr.Repo, "number", pr.Number, "pr", pr.ID, "err", rerr)
		}
	}
	return going, why
}

// reviewGoingErr is reviewGoing, also reporting a failed read of the gate
// setting, the review runs or the audits, for a caller that must not start
// a round on a read it couldn't make (WP-748).
func (s *Service) reviewGoingErr(ctx context.Context, pr *PullRequest) (bool, string, error) {
	on, _, err := s.gateConfigErr(ctx, pr.ForgeID, pr.Repo)
	if err != nil {
		return false, "", err
	}
	if on && (pr.GateState == "" || pr.GateState == GatePending || pr.gateSHA != pr.HeadSHA) {
		return true, "the review gate hasn't been evaluated for this head", nil
	}
	var running, queued int
	if err := s.db.R.QueryRowContext(ctx, `SELECT
		COALESCE(SUM(CASE WHEN status = 'running' THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN status = 'queued' THEN 1 ELSE 0 END), 0)
		FROM bot_runs WHERE pr_id = ? AND status IN ('queued','running')`, pr.ID).Scan(&running, &queued); err != nil {
		return false, "", err
	}
	if running > 0 {
		return true, "a review is running", nil
	}
	id, going, err := s.auditGoingErr(ctx, pr.ID)
	if err != nil {
		return false, "", err
	}
	if going {
		return true, fmt.Sprintf("audit #%d%s is running", id, s.auditKind(ctx, id)), nil
	}
	if queued == 0 {
		return false, "", nil
	}
	if s.reviewWaitOver(ctx, pr) {
		return false, "", nil
	}
	return true, "a review is queued", nil
}

// reviewWaitOver says whether the repository's review wait, counted from when
// the head was first seen, has passed (false when it has none).
func (s *Service) reviewWaitOver(ctx context.Context, pr *PullRequest) bool {
	pol, ok, err := s.repoMergePolicy(ctx, pr.ForgeID, pr.Repo)
	if err != nil || !ok || pol.ReviewWaitMinutes <= 0 {
		return false
	}
	since := pr.UpdatedAt
	if pr.headAt != nil {
		since = *pr.headAt
	}
	return !s.now().Before(since.Add(time.Duration(pol.ReviewWaitMinutes) * time.Minute))
}

// mergedSHA is the commit a merged pull request became, as its forge says
// (empty when it doesn't).
func (s *Service) mergedSHA(ctx context.Context, pr *PullRequest) string {
	f, err := s.forgeByID(ctx, pr.ForgeID)
	prc, ok := s.Forges.(ForgePRs)
	if err != nil || !ok || f.token == "" {
		return ""
	}
	fp, err := prc.GetPR(ctx, f, pr.Repo, pr.Number)
	if err != nil {
		return ""
	}
	return fp.MergeSHA
}

// failedNotCancelled says whether a check failed other than by being
// cancelled.
func failedNotCancelled(checks []PRCheck) bool {
	return slices.ContainsFunc(checks, func(c PRCheck) bool {
		return c.State == CheckFailed && !cancelledCheck(c.Description)
	})
}

// cancelledCheck: Forgejo reports a cancelled job as a failed status "Has
// been cancelled"; GitHub's check runs say "cancelled".
func cancelledCheck(description string) bool {
	return strings.Contains(strings.ToLower(description), "cancel")
}

// failedThenStopped is the description Waypoint gives a check the forge
// calls cancelled when its own log shows a step failed first (WP-424).
const failedThenStopped = "Failed (a step failed; the job then stopped its run)"

// stepFailedRE is the Forgejo runner's line for a step that exited non-zero.
// A job cancelled from outside (a newer push, a person) ends on
// "[runner]: context canceled" instead.
var stepFailedRE = lazyre.New(`\[runner\]: exitcode '[1-9][0-9]*': failure`)

// goTimeoutRE is go test's panic when a test binary runs past its -timeout
// (WP-923): the package's whole run took too long, whichever test it was on.
var goTimeoutRE = lazyre.New(`panic: test timed out after [0-9][0-9a-zµ.]*`)

// logMarks are the lines a stored job log keeps when its tail cuts them.
var logMarks = []*lazyre.Regexp{stepFailedRE, goTimeoutRE}

// stepFailedLog says whether a job's log shows one of its own steps failed.
// A workflow that fails fast (a step that cancels the run when a job fails,
// as some repos' do) has the forge mark the failing job itself cancelled; its
// log tells it from a job that was only stopped, so no re-run is wasted on
// a real failure and the agent gets a round to fix it (WP-424).
func stepFailedLog(log string) bool {
	return stepFailedRE.MatchString(log)
}
