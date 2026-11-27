package core

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"time"
)

// reasonConflict is why a pull request that conflicts with its base can't merge.
func reasonConflict(base string) string {
	return "has conflicts with " + cmp(base, "its base")
}

// queueable: what keeps pr from merging is only what the line deals with:
// being a Waypoint draft, or being behind its base. A conflict is a round's.
func queueable(pr *PullRequest, reasons []string) bool {
	if pr.ArmedAt == nil || len(reasons) == 0 {
		return false
	}
	for _, r := range reasons {
		switch r {
		case reasonBehind(pr.BaseRef):
		case reasonDraft:
			if !pr.WipAuto {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// queueAhead lists the pull requests ahead of pr in its line, front first;
// pr not in the line is behind them all. The order is the line's own
// (lineOrder), so the pull request that fixes a red base isn't told to wait
// behind ones the line puts it ahead of; what fails on the base is read at
// the tip Waypoint last stored, which costs no call to the forge while the
// base is green. While it is red, a queued head that passes a job failing on
// the base costs one Contains call to the forge, cached per tip and head once
// it answers (a call that fails is asked again).
func (s *Service) queueAhead(ctx context.Context, pr *PullRequest) ([]*PullRequest, error) {
	members, err := s.lineMembers(ctx, lineOf(pr))
	if err != nil {
		return nil, err
	}
	var all []*PullRequest
	for _, p := range members {
		if p.ID != pr.ID {
			all = append(all, p)
		}
	}
	if pr.QueuedAt != nil {
		all = append(all, pr)
	}
	if _, _, err := s.lineOrder(ctx, all, ""); err != nil {
		return nil, err
	}
	if pr.QueuedAt == nil {
		return all, nil
	}
	return all[:slices.Index(all, pr)], nil
}

// queueHolder: Waypoint marked it ready, so it holds the front of its queue.
func queueHolder(pr *PullRequest) bool {
	return pr.WipAuto && pr.State == "open" && pr.ReadyAt != nil
}

func ordinal(n int) string {
	suffix := "th"
	switch {
	case n%100 >= 11 && n%100 <= 13:
	case n%10 == 1:
		suffix = "st"
	case n%10 == 2:
		suffix = "nd"
	case n%10 == 3:
		suffix = "rd"
	}
	return fmt.Sprintf("%d%s", n, suffix)
}

// leaveQueue takes pr out of its merge queue: it has work to do first, and
// joins again at its place in the line once that is done.
//
// It takes the line's lock (WP-1706), so a step that is inside its forge
// calls for the line finishes before the pull request is out of it. A caller
// that holds the lock already (the step's own drop, a babysit round under
// readyCatchUp) carries it in ctx and passes straight through.
func (s *Service) leaveQueue(ctx context.Context, pr *PullRequest) {
	ctx, unlock := s.queueLockCtx(ctx, pr)
	defer unlock()
	if _, err := s.db.W.ExecContext(ctx, `UPDATE pull_requests SET queued_at = NULL WHERE id = ? AND queued_at IS NOT NULL`, pr.ID); err != nil {
		s.log.WarnContext(ctx, "leave the merge queue", "pr", pr.ID, "err", err)
	}
}

// faultFrontHold is how long a pull request whose checks failed on the CI
// runner keeps its place in the merge queue: long enough for Waypoint's own
// re-runs on a host that recovers (faultRerunSettle apart), not for a fix
// someone has to make. One project's line stood still for over five hours on
// 2026-11-10 behind a pull request that waited on a task held for a
// person.
const faultFrontHold = 15 * time.Minute

// yieldToRunnerFault takes pr out of its merge queue once its head has
// failed on the CI runner for faultFrontHold, and moves the queue on: the
// pull request behind it gets its turn. pr's babysit goes on waiting for
// the re-run or the fix, and joins again at its place in the line
// (the line takes it back) once its checks pass, as after a round. Its first failure on
// this head is in its fault re-run record (rerunRunnerFaults).
func (s *Service) yieldToRunnerFault(ctx context.Context, b *Babysit, pr *PullRequest) {
	if pr.QueuedAt == nil {
		return
	}
	st := parseFaultRerun(b.faultReruns)
	if st.sha != pr.HeadSHA || st.first == 0 || s.now().Sub(msTime(st.first)) < faultFrontHold {
		return
	}
	s.leaveQueue(ctx, pr)
	pr.QueuedAt = nil
	s.log.Info("a pull request failing on the CI runner gave up its turn in the merge queue", "pr", pr.ID, "head", pr.HeadSHA)
	s.nudgeLine(ctx, pr)
}

// queueStage is a pull request's place in its merge line for the page and
// `wp pr show`: the wait the line put on its babysit, "" when it is at the
// front or not in the line.
func (s *Service) queueStage(ctx context.Context, pr *PullRequest) string {
	if pr.QueuedAt == nil || (pr.State != "open" && pr.State != "draft") {
		return ""
	}
	b, err := s.activeBabysit(ctx, s.db.R, pr.ID)
	if err != nil || b == nil || !strings.HasPrefix(b.Wait, "in the merge line") {
		return ""
	}
	return strings.TrimPrefix(b.Wait, "in the ")
}

// queueStates are the babysit states that hold a place in a merge queue.
var queueStates = []string{BabysitWatching, BabysitWaitingReview, BabysitReady, BabysitWorking}

// heldLinesKey keys the line locks a context's goroutine holds.
type heldLinesKey struct{}

// heldLine is one held line lock: released once its holder unlocks.
type heldLine struct {
	key      string
	released atomic.Bool
}

// queueLockCtx serializes the decisions about one merge queue. The lock is
// not reentrant, so the context it returns carries a marker for the line:
// pass it to everything called while holding, and a leaveQueue (or another
// queueLockCtx) on it passes through instead of waiting on itself (WP-1706).
// A marker is good only while its holder holds: unlocking marks it released,
// so a context that outlived the holder locks normally. A context used on
// another goroutine should still go through withoutLineLocks, because a
// marker that is live there skips a lock that goroutine doesn't hold.
func (s *Service) queueLockCtx(ctx context.Context, pr *PullRequest) (context.Context, func()) {
	key := fmt.Sprintf("queue:%d:%s:%s", pr.ForgeID, strings.ToLower(pr.Repo), pr.BaseRef)
	held, _ := ctx.Value(heldLinesKey{}).([]*heldLine)
	if slices.ContainsFunc(held, func(h *heldLine) bool { return h.key == key && !h.released.Load() }) {
		return ctx, func() {}
	}
	unlock := forgeLocks.lock(key)
	h := &heldLine{key: key}
	return context.WithValue(ctx, heldLinesKey{}, append(slices.Clone(held), h)), func() {
		h.released.Store(true)
		unlock()
	}
}

// withoutLineLocks is ctx for work that runs on another goroutine: the
// holder's locks aren't that goroutine's.
func withoutLineLocks(ctx context.Context) context.Context {
	return context.WithValue(ctx, heldLinesKey{}, []*heldLine(nil))
}

// lineKey is a pull request's place in its line: when it was armed.
func lineKey(pr *PullRequest) int64 {
	if pr.ArmedAt != nil {
		return pr.ArmedAt.UnixMilli()
	}
	return 0
}

// lineAhead counts the pull requests ahead of pr in its line: armed, with a
// babysit that moves them on, and marked ready by Waypoint or earlier in
// the line.
func (s *Service) lineAhead(ctx context.Context, pr *PullRequest) (int, error) {
	key := lineKey(pr)
	var n int
	err := s.db.R.QueryRowContext(ctx, `SELECT count(*) FROM pull_requests p
		WHERE p.forge_id = ? AND p.repo = ? COLLATE NOCASE AND p.base_ref = ? AND p.id != ? AND p.state IN ('open','draft')
		  AND p.armed_at IS NOT NULL AND p.outside = 0
		  AND EXISTS (SELECT 1 FROM babysits b WHERE b.pr_id = p.id AND b.ended_at IS NULL AND b.state IN (?, ?, ?, ?))
		  AND ((p.wip_auto = 1 AND p.state = 'open' AND p.ready_at IS NOT NULL)
		    OR p.armed_at < ? OR (p.armed_at = ? AND p.number < ?))`,
		pr.ForgeID, pr.Repo, pr.BaseRef, pr.ID, queueStates[0], queueStates[1], queueStates[2], queueStates[3], key, key, pr.Number).Scan(&n)
	return n, err
}

// lineFrontPR is SQL true for a pull_requests row of alias p that is at the
// front of its merge line: armed, in the line, and either the holder
// (queueHolder) or with no other pull request ahead of it by the rule
// lineAhead counts with (WP-1618, WP-ADR149). Keep the two in agreement.
var lineFrontPR = `(p.armed_at IS NOT NULL AND p.outside = 0 AND p.state IN ('open','draft')
	AND ((p.wip_auto = 1 AND p.state = 'open' AND p.ready_at IS NOT NULL)
	  OR NOT EXISTS (SELECT 1 FROM pull_requests q
		WHERE q.forge_id = p.forge_id AND q.repo = p.repo COLLATE NOCASE AND q.base_ref = p.base_ref AND q.id != p.id AND q.state IN ('open','draft')
		  AND q.armed_at IS NOT NULL AND q.outside = 0
		  AND EXISTS (SELECT 1 FROM babysits b2 WHERE b2.pr_id = q.id AND b2.ended_at IS NULL AND b2.state IN ('` + strings.Join(queueStates, "','") + `'))
		  AND ((q.wip_auto = 1 AND q.state = 'open' AND q.ready_at IS NOT NULL)
		    OR q.armed_at < p.armed_at
		    OR (q.armed_at = p.armed_at AND q.number < p.number)))))`

// nudgeLine has the line look again after pr left it (merged, closed, sent to
// a round): the next goes without waiting for the sweep. A merge also has the
// forge read the drafts behind it again (rereadQueue). The line looks in the
// background: callers may hold the base's queue lock (readyCatchUp, the
// shared timeout), which a step of the line takes too.
func (s *Service) nudgeLine(ctx context.Context, pr *PullRequest) {
	s.rereadQueue(ctx, pr)
	s.lineChangedAsync(lineOf(pr))
}

// rereadQueueMax bounds the pull requests rereadQueue reads after a merge.
const rereadQueueMax = 20

// rereadQueue has the forge read again, soon, whether the drafts queued
// behind pr's place still merge with the base (WP-510): a merge into the
// base can make them conflict, and the forge's mergeable flag is the early
// warning. The read waits mergeConfirmAfter, for the forge to test them
// against the new base; a "no" it gives is confirmed by the usual rechecks
// (WP-138). Forgejo's and Gitea's drafts are titled work in progress, and for
// those it answers no whatever the truth (WP-171): their conflict shows in a
// candidate's run.
func (s *Service) rereadQueue(ctx context.Context, pr *PullRequest) {
	if pr.State != "merged" {
		return
	}
	if f, err := s.forgeByID(ctx, pr.ForgeID); err != nil || f.Kind == "forgejo" || f.Kind == "gitea" {
		return
	}
	at := s.now().Add(mergeConfirmAfter).UnixMilli()
	if _, err := s.db.W.ExecContext(ctx, `UPDATE pull_requests SET recheck_at = ? WHERE id IN (
		SELECT p.id FROM pull_requests p WHERE p.forge_id = ? AND p.repo = ? COLLATE NOCASE AND p.base_ref = ? AND p.id != ?
		AND p.state = 'draft' AND p.wip_auto = 1 AND p.armed_at IS NOT NULL AND p.queued_at IS NOT NULL AND p.recheck_at IS NULL
		ORDER BY p.armed_at, p.number LIMIT ?)`, at, pr.ForgeID, pr.Repo, pr.BaseRef, pr.ID, rereadQueueMax); err != nil {
		s.log.WarnContext(ctx, "read the merge line's drafts again", "pr", pr.ID, "err", err)
	}
}
