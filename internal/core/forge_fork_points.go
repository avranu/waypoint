package core

// Updates behind the front of a merge queue: a pull request whose head forks
// from its base at more than one commit (WP-467), and one whose run still
// waits for a runner (WP-609). See mergeBaseBehindFront.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
)

// ForgeForkPoints is the optional part of a ForgeClient that tells whether a
// head forks from a base's tip at more than one commit.
type ForgeForkPoints interface {
	ForkedTwice(ctx context.Context, f *Forge, repo, tip, head string) (bool, error)
}

// ForkedTwice compares two counts of head's own commits. Forgejo's compare
// diffs from the one merge base it picks, so it also counts a commit head
// shares with tip that isn't in that merge base's history, and there is such
// a commit exactly when there is another merge base. commits?not= lists what
// git gives for tip..head, whichever the merge bases are. The list is counted
// rather than its total header, which is a claim about a page: reading it
// wrong would update every pull request on every push to the base.
func (c *httpForgeClient) ForkedTwice(ctx context.Context, f *Forge, repo, tip, head string) (bool, error) {
	if f.Kind == "gitlab" || f.Kind == "github" {
		return false, fmt.Errorf("%w: only Forgejo and Gitea", ErrUnsupported)
	}
	shown, err := c.Ahead(ctx, f, repo, tip, head)
	if err != nil {
		return false, err
	}
	own := 0
	for page := 1; own < shown; page++ {
		var list []struct {
			SHA string `json:"sha"`
		}
		path := fmt.Sprintf("/repos/%s/commits?sha=%s&not=%s&page=%d&limit=50&stat=false&verification=false&files=false", repo, escapePath(head), escapePath(tip), page)
		if err := c.do(ctx, f, http.MethodGet, path, nil, &list); err != nil {
			return false, err
		}
		if len(list) == 0 {
			break
		}
		own += len(list)
	}
	return own > 0 && own < shown, nil // none listed: the forge didn't answer the question
}

// mergeBaseBehindFront brings an armed pull request behind the front of its
// merge queue up to date with its base in two cases; WP-510 leaves the rest
// as they are until their turn. Called after each push to the base (and by
// the sync's sweep for missed ones), since that push is what puts a head
// behind.
//
//   - Its head forks from the base at more than one commit (WP-467), which
//     is what a branch stacked on another that also merged the base in
//     comes to once the base has merged the lower branch. The forge then
//     diffs from one of those commits, often the merge of the base, and
//     shows what the lower branches changed as this pull request's change,
//     so its reviewers review other pull requests' code. Merging the base
//     in leaves its tip the only fork point. Costs one draft-tier run.
//   - Its head's run hasn't started: every job still waits for a runner
//     (WP-609, WP-ADR116). The update's push replaces that run (the workflow's
//     concurrency group cancels the waiting one), so it costs no runner
//     time, and the run the head gets tests the newest base: a fix that
//     landed meanwhile isn't failed on again, and a conflict shows before
//     the pull request's turn. A run a runner has taken, or that finished,
//     is left alone: updating then costs a run, which WP-510 stopped. This
//     assumes the repo's workflow cancels a pull request's superseded runs
//     (Forgejo's default, kept by ci.yml); one that queues them instead pays
//     a run per update.
//
// The forge is asked outside the locks, since this runs for every armed pull
// request on the base and the front's queue step needs the queue's lock. So
// the pull request is judged again under them, on the row as it is then: a
// round starts under the babysit's lock and ready is marked under the queue's.
func (s *Service) mergeBaseBehindFront(ctx context.Context, f *Forge, id int64) {
	pr := s.mergeBaseCandidate(ctx, id)
	if pr == nil {
		return
	}
	run := s.headRun(ctx, pr)
	tip, behind, stale := s.behindBase(ctx, pr)
	if !behind || stale {
		return
	}
	base := cmp(pr.BaseRef, "its base")
	var note string
	waiting := false
	switch {
	case s.forkedTwice(ctx, f, pr, tip):
		note = "forked from " + base + " at more than one commit, so its diff showed other pull requests' changes; merged " + base + " in, waiting for checks"
	case (run == runWaiting || run == runUnread) && s.runWaitingNow(ctx, f, pr):
		note, waiting = "its checks were still waiting for a runner, so "+base+" was merged in first: the run tests the newest "+base+" at no extra runner time", true
	default:
		return
	}
	defer s.babysitLocks.lock(id)()
	defer s.queueLock(pr)()
	now := s.mergeBaseCandidate(ctx, id)
	if now == nil || !sameSHA(now.HeadSHA, pr.HeadSHA) {
		return
	}
	if waiting && !s.runWaitingNow(ctx, f, now) {
		return // a runner took it meanwhile: read from the forge again, not the recorded rows
	}
	if s.updateIfBehind(ctx, now) == upUpdated {
		s.armNote(ctx, id, note)
	}
}

// forkedTwice: pr's head forks from tip at more than one commit (WP-467).
func (s *Service) forkedTwice(ctx context.Context, f *Forge, pr *PullRequest, tip string) bool {
	cl, ok := s.Forges.(ForgeForkPoints)
	if !ok {
		return false
	}
	twice, err := cl.ForkedTwice(ctx, f, pr.Repo, tip, pr.HeadSHA)
	if err != nil && !errors.Is(err, ErrUnsupported) {
		s.log.WarnContext(ctx, "tell whether a head forks from its base twice", "repo", pr.Repo, "number", pr.Number, "err", err)
	}
	return err == nil && twice
}

// Where a head's Actions run stands, as headRun reads it.
const (
	runUnread  = iota // no check recorded on it: its run may not have posted yet
	runNone           // no Actions job waits (another app's checks only, or each skipped)
	runWaiting        // every job waits for a runner, or skipped without one
	runStarted        // a runner has a job of it, or a job finished
)

// waitingJob: an Actions job's status while it waits for a runner, or for
// the jobs it needs, as Forgejo and Gitea word it.
func waitingJob(c PRCheck) bool {
	return c.State == CheckPending && (c.Description == "Waiting to run" || c.Description == "Blocked by required conditions")
}

// headRun reads from pr's head's recorded checks whether its Actions run has
// started. Only Actions jobs count (their status points at a run of the
// repo): a review bot's pending status says nothing about runners. A job
// skipped by its condition took no runner. A head's jobs only move on, so a
// recorded start is final and costs no forge read.
func (s *Service) headRun(ctx context.Context, pr *PullRequest) int {
	checks, err := s.prChecks(ctx, pr)
	if err != nil {
		return runStarted // can't tell: leave it
	}
	waiting := 0
	for _, c := range checks {
		if _, ok := fgStatusRun(c.URL, pr.Repo); !ok {
			continue
		}
		switch {
		case skippedCheck(CommitCheck{State: c.State, Description: c.Description}):
		case waitingJob(c):
			waiting++
		default:
			return runStarted
		}
	}
	switch {
	case waiting > 0:
		return runWaiting
	case len(checks) == 0:
		// A head just pushed, or read before its run posted: the forge is
		// asked, but only once the base has moved past it.
		return runUnread
	}
	return runNone
}

// runWaitingNow reads pr's head's checks from the forge again and says
// whether its run still waits for a runner: the recorded ones may be minutes
// old, and a run a runner has taken isn't cancelled for an update.
//
// The checks alone can't say so: they take each job's state from the head's
// newest run, and an edit of a ready pull request queues a run behind the one
// testing the head (ci.yml's concurrency block, WP-353), whose jobs read
// "Blocked by required conditions" while the older run is still going. So
// every run of the head is read too, and one a runner has, or that finished,
// leaves the head alone. A forge that can't list them gets no update.
func (s *Service) runWaitingNow(ctx context.Context, f *Forge, pr *PullRequest) bool {
	if err := s.refreshHead(ctx, f, pr.Repo, pr.HeadSHA); err != nil {
		s.log.WarnContext(ctx, "read a head's checks", "repo", pr.Repo, "number", pr.Number, "err", err)
		return false
	}
	if s.headRun(ctx, pr) != runWaiting {
		return false
	}
	cl, ok := s.Forges.(ForgeActionLogs)
	if !ok {
		return false
	}
	runs, err := cl.ActionRunsAt(ctx, f, pr.Repo, pr.HeadSHA)
	if err != nil {
		s.log.WarnContext(ctx, "list a head's Actions runs", "repo", pr.Repo, "number", pr.Number, "err", err)
		return false
	}
	return runsWaiting(runs, pr.HeadSHA)
}

// runsWaiting: of the runs of head, at least one waits for a runner (or for
// another run) and none is running or finished. Skipped and cancelled runs
// hold no runner now.
func runsWaiting(runs []ActionRun, head string) bool {
	waits := false
	for _, r := range runs {
		switch {
		case !sameSHA(r.CommitSHA, head), r.Status == "skipped", runCancelled(r.Status):
		case r.Status == "waiting" || r.Status == "blocked":
			waits = true
		default:
			return false // running or finished: a runner had it
		}
	}
	return waits
}

// mergeBaseCandidate is the pull request as it stands now, when
// mergeBaseBehindFront may update it, else nil. The front is left to the
// queue, which updates it anyway; so are a head under a round's agent (the
// update would race its push), one marked ready (its full run is going), one
// a live stack tests, and one that closed or merged.
func (s *Service) mergeBaseCandidate(ctx context.Context, id int64) *PullRequest {
	pr, err := s.prByID(ctx, s.db.R, id)
	if err != nil || (pr.State != "open" && pr.State != "draft") || pr.ArmedAt == nil || pr.ReadyAt != nil || pr.Outside || pr.HeadSHA == "" {
		return nil
	}
	b, err := s.activeBabysit(ctx, s.db.R, id)
	if err != nil || b == nil || b.State == BabysitWorking || !slices.Contains(queueStates, b.State) {
		return nil
	}
	if pr.QueuedAt != nil {
		if ahead, err := s.queueAhead(ctx, pr); err != nil || len(ahead) == 0 {
			return nil
		}
	}
	if s.stackHolds(ctx, pr) {
		return nil
	}
	return pr
}
