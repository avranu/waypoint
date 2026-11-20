package core

// The merge queue (WP-222, WP-ADR7). Armed pull requests to one base
// of one repo merge one at a time, in the order they joined. Only the front
// of the queue is brought up to date with its base, has a conflict sent to a
// round, and (draft-first, WP-158) is marked ready for its full run; the
// others wait as they are. Before the queue every merge to the base sent
// every other armed pull request back through a base merge and a full run,
// so runs grew with the square of the open pull requests, and a full run
// was spent on each head that went ready whether or not it ever merged.
// Now a merge costs the next pull request one update and one full run.
//
// A pull request joins when nothing keeps it from merging but what its turn
// deals with: being a draft, a conflict, being behind its base (queueable).
// It keeps its place across pushes (Waypoint's own update of its branch is
// one), through a round for a conflict and while a person is asked about
// it; it leaves when a round is handed out for other work (a failed check, a
// comment, a failed full run) or when it is disarmed, merged or closed. The
// front marked ready keeps its place and stays ready through a round a runner
// agent takes, unless the failure is the full run of a head a round pushed
// (WP-455, WP-456, WP-ADR31): its push runs the full tier at once instead of
// a draft tier followed by one. The front is a pull request marked ready (it
// holds the queue until it merges or goes back to draft), else the one that
// joined first; a draft front is marked ready just before Waypoint brings it
// up to date, so the update's push runs the full tier. A pull request whose
// babysit waits for a person holds no place, so it never stalls the queue;
// one at work on a conflict round keeps its own. Nor does one whose checks
// failed on the CI runner for longer than faultFrontHold (WP-837): it waits
// for a re-run or a fix elsewhere, and the rest of the line doesn't wait
// with it.
//
// The line (WP-428, WP-ADR29): every armed pull request with a babysit that moves it
// on stands in its base's line by when it was armed, queued or not, and
// joins (or joins again) the queue at that place, not at the back; one that
// fixes what fails on its base's tip goes first (base_red.go). Rounds go
// only to the roundWindow nearest the front: a round far back costs a draft
// run and a review on a head that will be brought up to date before it
// merges, and when the base is red every pull request fails alike. Before
// this, a round sent a pull request to the back, and with fifty in a line a
// pull request that needed two rounds waited through it three times while
// the rounds' pushes kept every CI runner busy.

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"
)

// reasonConflict is why a pull request that conflicts with its base can't merge.
func reasonConflict(base string) string {
	return "has conflicts with " + cmp(base, "its base")
}

// queueable: what keeps pr from merging is only what its turn at the front
// of the merge queue deals with.
func queueable(pr *PullRequest, reasons []string) bool {
	if pr.ArmedAt == nil || len(reasons) == 0 {
		return false
	}
	for _, r := range reasons {
		switch r {
		case reasonConflict(pr.BaseRef), reasonBehind(pr.BaseRef):
		case reasonDraft:
			if !pr.WipAuto {
				return false // a person's draft: theirs to mark ready
			}
		default:
			return false
		}
	}
	return true
}

// queueAhead lists the pull requests ahead of pr in its merge queue, front
// first; pr not in the queue is behind them all.
func (s *Service) queueAhead(ctx context.Context, pr *PullRequest) ([]*PullRequest, error) {
	// #nosec G202 -- a fixed column list; values are bound
	rows, err := s.db.R.QueryContext(ctx, `SELECT `+prColsAs("p")+` FROM pull_requests p
		WHERE p.forge_id = ? AND p.repo = ? COLLATE NOCASE AND p.base_ref = ? AND p.id != ? AND p.state IN ('open','draft')
		  AND p.armed_at IS NOT NULL AND p.queued_at IS NOT NULL
		  AND EXISTS (SELECT 1 FROM babysits b WHERE b.pr_id = p.id AND b.ended_at IS NULL AND b.state IN (?, ?, ?, ?))
		ORDER BY (p.wip_auto = 1 AND p.state = 'open' AND p.ready_at IS NOT NULL) DESC, p.queued_at, p.number`,
		pr.ForgeID, pr.Repo, pr.BaseRef, pr.ID, queueStates[0], queueStates[1], queueStates[2], queueStates[3])
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var all []*PullRequest
	for rows.Next() {
		p, err := scanPR(rows)
		if err != nil {
			return nil, err
		}
		all = append(all, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var ahead []*PullRequest
	for _, p := range all {
		switch {
		case queueHolder(p) && queueHolder(pr):
			// Two marked ready (one came back from a person): the first goes.
			if p.ReadyAt.Before(*pr.ReadyAt) || (p.ReadyAt.Equal(*pr.ReadyAt) && p.Number < pr.Number) {
				ahead = append(ahead, p)
			}
		case queueHolder(p):
			ahead = append(ahead, p)
		case queueHolder(pr):
		case pr.QueuedAt == nil || p.QueuedAt.Before(*pr.QueuedAt) || (p.QueuedAt.Equal(*pr.QueuedAt) && p.Number < pr.Number):
			ahead = append(ahead, p)
		}
	}
	return ahead, nil
}

// queueHolder: Waypoint marked it ready, so it holds the front of its queue.
func queueHolder(pr *PullRequest) bool {
	return pr.WipAuto && pr.State == "open" && pr.ReadyAt != nil
}

// behindFront is the step of a pull request waiting behind the front of its
// merge queue, and returns the wait to show. Near the front it is tested on
// a stack (WP-404) where it can be; otherwise it waits as it is, with no
// push and no run (WP-510). The early warning of a conflict comes from the
// forge's mergeable flag, read again after each merge into the base
// (rereadQueue), or from a stack's run on Forgejo, which has no flag for a
// work-in-progress draft (stackBaseConflict): it is shown here, and further back than conflictEarly (or
// with no runner free) the round for it is the front's job when its turn
// comes, as before.
func (s *Service) behindFront(ctx context.Context, pr *PullRequest, rd *PRReadiness, wait string) string {
	if st := s.stackBehind(ctx, pr); st != "" {
		return wait + "; " + st
	}
	if ahead, err := s.queueAhead(ctx, pr); err == nil && stackPlace(pr, len(ahead)) && s.batching(ctx, pr) {
		return wait + "; the runners are busy, so it waits for a merge queue batch"
	}
	if (rd != nil && slices.Contains(rd.Reasons, reasonConflict(pr.BaseRef))) || s.updateConflicted(pr) {
		return wait + "; " + reasonConflict(pr.BaseRef) + ", resolved by a round when its turn at the front comes"
	}
	return wait
}

// conflictEarly: the places right behind the front of a merge queue whose
// conflict with the base goes to a round at once (WP-457), while the front's
// full run goes. Before, the conflict waited for the pull request's turn at
// the front, so its round, draft tier and full run all came after the
// front's merge, while nothing else merged (#188 on 2026-11-02 sat 2nd with
// a known conflict through #186's whole full run). Resolved against the
// current base, the update at its turn is usually a clean merge; one the
// front's merge conflicts again gets the front's conflict round as before.
// Further back the base moves again before its turn, so it waits.
const conflictEarly = 2

// earlyConflict: pr waits within conflictEarly places behind the front of
// its queue, conflicts with its base (the forge's confirmed flag, or the
// line's update of this head refused at the base's current tip:
// conflictOnTip, which a stack's run feeds too, stackBaseConflict), and a
// round for it can go to a runner now. A head that already had its conflict round waits for its turn at the
// front, where the ladder goes on; with no runner free, or its owner's
// usage limit spent, it waits as it is, not handed to a person over a
// conflict the front's turn deals with.
func (s *Service) earlyConflict(ctx context.Context, b *Babysit, pr *PullRequest, rd *PRReadiness) bool {
	if b == nil || pr.Outside || pr.ArmedAt == nil || queueHolder(pr) {
		return false
	}
	flagged := rd != nil && slices.Contains(rd.Reasons, reasonConflict(pr.BaseRef))
	if !flagged && !s.updateConflicted(pr) {
		return false
	}
	if slices.ContainsFunc(b.Rounds, func(r BabysitRound) bool { return r.Conflict && sameSHA(r.SHA, pr.HeadSHA) }) {
		return false
	}
	// A round that couldn't start now would cost it its place (babysitRound),
	// and one that pushes would throw away the stack testing this head.
	if s.limitWait(b) != "" || s.stackHolds(ctx, pr) {
		return false
	}
	ahead, err := s.queueAhead(ctx, pr)
	if err != nil || len(ahead) == 0 || len(ahead) > conflictEarly {
		return false
	}
	if s.roundTurn(ctx, pr) != "" {
		return false // within the line's round window too (WP-428)
	}
	if runners, err := s.ownerRunners(ctx, b.ownerID); err != nil || !slices.ContainsFunc(runners, claudeReady) {
		return false
	}
	if !flagged && !s.conflictOnTip(ctx, pr) { // the forge reads: last
		return false
	}
	why, transient := s.babysitGateRead(ctx, pr)
	return why == "" && !transient // a failed read keeps it waiting
}

// stackBehind is the stack step of a draft waiting behind the front: it is
// tested on a stack, not brought up to date. "" when there is no live stack.
// A draft whose review is still to come gets no new stack (WP-ADR51; WP-489
// gave it one), but a live one it has is still followed.
func (s *Service) stackBehind(ctx context.Context, pr *PullRequest) string {
	ahead, err := s.queueAhead(ctx, pr)
	if err != nil || !stackPlace(pr, len(ahead)) {
		return ""
	}
	return s.stackStep(ctx, pr, ahead)
}

// queueFront: pr is at the front of its merge queue (or would be, joining
// now). When the queue can't be read it is: a merge goes on as before.
func (s *Service) queueFront(ctx context.Context, pr *PullRequest) bool {
	ahead, err := s.queueAhead(ctx, pr)
	if err != nil {
		s.log.WarnContext(ctx, "read the merge queue", "pr", pr.ID, "err", err)
		return true
	}
	return len(ahead) == 0
}

// queueTurn puts pr in its merge queue (keeping its place if it has one)
// and says what it waits for there; "" when it is at the front.
func (s *Service) queueTurn(ctx context.Context, pr *PullRequest) string {
	if pr.QueuedAt == nil {
		at := s.now()
		if pr.ArmedAt != nil && pr.ArmedAt.Before(at) {
			at = *pr.ArmedAt // its place in the line (WP-428)
		}
		if _, err := s.db.W.ExecContext(ctx, `UPDATE pull_requests SET queued_at = coalesce(queued_at, ?) WHERE id = ?`, at.UnixMilli(), pr.ID); err != nil {
			s.log.WarnContext(ctx, "join the merge queue", "pr", pr.ID, "err", err)
			return ""
		}
		pr.QueuedAt = &at
	}
	s.fixFirst(ctx, pr)
	s.urgentFirst(ctx, pr)
	ahead, err := s.queueAhead(ctx, pr)
	if err != nil {
		s.log.WarnContext(ctx, "read the merge queue", "pr", pr.ID, "err", err)
		return ""
	}
	if len(ahead) == 0 {
		return ""
	}
	return queueWait(pr, ahead)
}

// queueWait says where pr stands in its queue: "in the merge queue for main:
// 3rd, behind #33 (its full run) and #35".
func queueWait(pr *PullRequest, ahead []*PullRequest) string {
	var names []string
	for i, p := range ahead {
		if i == 3 {
			names = append(names, fmt.Sprintf("%d more", len(ahead)-3))
			break
		}
		n := fmt.Sprintf("#%d", p.Number)
		if queueHolder(p) {
			n += " (its full run)"
		}
		names = append(names, n)
	}
	return fmt.Sprintf("in the merge queue for %s: %s, behind %s", cmp(pr.BaseRef, "its base"), ordinal(len(ahead)+1), joinAnd(names))
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
func (s *Service) leaveQueue(ctx context.Context, prID int64) {
	if _, err := s.db.W.ExecContext(ctx, `UPDATE pull_requests SET queued_at = NULL WHERE id = ? AND queued_at IS NOT NULL`, prID); err != nil {
		s.log.WarnContext(ctx, "leave the merge queue", "pr", prID, "err", err)
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
// (queueTurn) once its checks pass, as after a round. Its first failure on
// this head is in its fault re-run record (rerunRunnerFaults).
func (s *Service) yieldToRunnerFault(ctx context.Context, b *Babysit, pr *PullRequest) {
	if pr.QueuedAt == nil {
		return
	}
	st := parseFaultRerun(b.faultReruns)
	if st.sha != pr.HeadSHA || st.first == 0 || s.now().Sub(msTime(st.first)) < faultFrontHold {
		return
	}
	s.leaveQueue(ctx, pr.ID)
	pr.QueuedAt = nil
	s.log.Info("a pull request failing on the CI runner gave up its turn in the merge queue", "pr", pr.ID, "head", pr.HeadSHA)
	s.nudgeQueue(ctx, pr)
}

// nudgeQueue moves the merge queue on when pr has left it (merged, closed,
// sent to a round): the next one goes without waiting for the sweep, and so
// do the stackDepth behind it, whose stacks change. The order is
// queueAhead's. The base moved under the rest, so whether they still merge
// is read again from the forge (rereadQueue).
func (s *Service) nudgeQueue(ctx context.Context, pr *PullRequest) {
	rows, err := s.db.R.QueryContext(ctx, `SELECT p.id FROM pull_requests p WHERE p.forge_id = ? AND p.repo = ? COLLATE NOCASE AND p.base_ref = ? AND p.id != ?
		AND p.state IN ('open','draft') AND p.armed_at IS NOT NULL AND p.queued_at IS NOT NULL
		AND EXISTS (SELECT 1 FROM babysits b WHERE b.pr_id = p.id AND b.ended_at IS NULL AND b.state IN (?, ?, ?, ?))
		ORDER BY (p.wip_auto = 1 AND p.state = 'open' AND p.ready_at IS NOT NULL) DESC, p.queued_at, p.number LIMIT ?`,
		pr.ForgeID, pr.Repo, pr.BaseRef, pr.ID, queueStates[0], queueStates[1], queueStates[2], queueStates[3], stackDepth+1)
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
		s.nudgeBabysit(id, false)
	}
	s.rereadQueue(ctx, pr)
}

// rereadQueueMax bounds the pull requests rereadQueue reads after a merge.
const rereadQueueMax = 20

// rereadQueue has the forge read again, soon, whether the drafts queued
// behind pr's place still merge with the base (WP-510): a merge into the
// base can make them conflict, and with no update pushed to them, the
// forge's mergeable flag is the early warning. The read waits
// mergeConfirmAfter, for the forge to test them against the new base; a
// "no" it gives is confirmed by the usual rechecks (WP-138), and a pull
// request already due a read keeps its time. Forgejo's and Gitea's drafts are titled
// work in progress, and for those it answers no whatever the truth
// (WP-171): with nothing to learn there, their conflict waits for the
// front's update (queueStep).
func (s *Service) rereadQueue(ctx context.Context, pr *PullRequest) {
	if pr.State != "merged" {
		return // the base didn't move
	}
	if f, err := s.forgeByID(ctx, pr.ForgeID); err != nil || f.Kind == "forgejo" || f.Kind == "gitea" {
		return
	}
	at := s.now().Add(mergeConfirmAfter).UnixMilli()
	if _, err := s.db.W.ExecContext(ctx, `UPDATE pull_requests SET recheck_at = ? WHERE id IN (
		SELECT p.id FROM pull_requests p WHERE p.forge_id = ? AND p.repo = ? COLLATE NOCASE AND p.base_ref = ? AND p.id != ?
		AND p.state = 'draft' AND p.wip_auto = 1 AND p.armed_at IS NOT NULL AND p.queued_at IS NOT NULL AND p.recheck_at IS NULL
		ORDER BY p.queued_at, p.number LIMIT ?)`, at, pr.ForgeID, pr.Repo, pr.BaseRef, pr.ID, rereadQueueMax); err != nil {
		s.log.WarnContext(ctx, "read the merge queue's drafts again", "pr", pr.ID, "err", err)
	}
}

// queueStage is a draft's place in its merge queue for the page and `wp pr
// show`, "" when it is at the front or not queued.
func (s *Service) queueStage(ctx context.Context, pr *PullRequest) string {
	if pr.QueuedAt == nil || queueHolder(pr) || (pr.State != "open" && pr.State != "draft") {
		return ""
	}
	ahead, err := s.queueAhead(ctx, pr)
	if err != nil || len(ahead) == 0 {
		return ""
	}
	return strings.TrimPrefix(queueWait(pr, ahead), "in the ")
}

// inQueue: the pull request holds a place in its merge queue, with a
// babysit that moves it on (not one waiting for a person).
func (s *Service) inQueue(ctx context.Context, pr *PullRequest) bool {
	if pr.QueuedAt == nil || pr.ArmedAt == nil {
		return false
	}
	b, err := s.activeBabysit(ctx, s.db.R, pr.ID)
	return err == nil && b != nil && slices.Contains(queueStates, b.State)
}

// queueStates are the babysit states that hold a place in a merge queue.
var queueStates = []string{BabysitWatching, BabysitWaitingReview, BabysitReady, BabysitWorking}

// queueLock serializes the decisions about one merge queue.
func (s *Service) queueLock(pr *PullRequest) func() {
	return forgeLocks.lock(fmt.Sprintf("queue:%d:%s:%s", pr.ForgeID, strings.ToLower(pr.Repo), pr.BaseRef))
}

// roundWindow: rounds go to the pull requests this near the front of their
// line (WP-428); further back, a round waits.
const roundWindow = 5

// lineKey is a pull request's place in its line: when it joined the queue,
// else when it was armed.
func lineKey(pr *PullRequest) int64 {
	switch {
	case pr.QueuedAt != nil:
		return pr.QueuedAt.UnixMilli()
	case pr.ArmedAt != nil:
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
		    OR coalesce(p.queued_at, p.armed_at) < ? OR (coalesce(p.queued_at, p.armed_at) = ? AND p.number < ?))`,
		pr.ForgeID, pr.Repo, pr.BaseRef, pr.ID, queueStates[0], queueStates[1], queueStates[2], queueStates[3], key, key, pr.Number).Scan(&n)
	return n, err
}

// roundTurn says what a round for pr waits for: "" when it goes now. Only
// an armed pull request waits, and not one marked ready (the front) or one
// labelled ci-urgent, which goes ahead of the line anyway (WP-686).
func (s *Service) roundTurn(ctx context.Context, pr *PullRequest) string {
	if pr.ArmedAt == nil || pr.Outside || queueHolder(pr) || pr.urgent() {
		return ""
	}
	n, err := s.lineAhead(ctx, pr)
	if err != nil {
		s.log.WarnContext(ctx, "read the merge line", "pr", pr.ID, "err", err)
		return ""
	}
	if n < roundWindow {
		return ""
	}
	return fmt.Sprintf("its round waits until it is within %d of the front of the line for %s: %s, %d ahead", roundWindow, cmp(pr.BaseRef, "its base"), ordinal(n+1), n)
}
