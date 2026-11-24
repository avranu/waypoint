package core

import (
	"slices"
	"strings"
	"testing"
)

// WP-1708: tests for what PR #569 (WP-1659) deleted without an equivalent.

// branchMode turns line_candidates off: the line tests the front on its own
// branch.
func (se *stackEnv) branchMode() {
	se.t.Helper()
	if _, err := se.s.db.W.ExecContext(se.ctx, `UPDATE project_repos SET line_candidates = 0`); err != nil {
		se.t.Fatal(err)
	}
}

// frontWithRunBehind brings #7 to the front the way production does: it joins
// green and on the tip, the line marks it ready (its full run starts, checks
// pending), then the base moves and its head lacks the tip.
func (se *stackEnv) frontWithRunBehind() {
	se.t.Helper()
	se.draftGreen(head7s, 5)
	se.step()
	se.line()
	if pr := se.pr(7); pr.ReadyAt == nil || pr.State != "open" {
		se.t.Fatalf("#7 not marked ready by the line: %+v", pr)
	}
	if u := se.sg.updatedPRs(); len(u) != 0 {
		se.t.Fatalf("branches updated while on the tip: %v", u)
	}
	se.sg.setChecks(head7s, CommitCheck{Name: "ci / backend (pull_request)", State: CheckPending, ID: 11}, CommitCheck{Name: "ci / e2e (pull_request)", State: CheckPending, ID: 12})
	se.poll()
	se.s.Wait()
	se.sg.behind = map[string]bool{head7s: true}
}

// WP-316 (catchUpFront before #569): a front that fell behind its base while
// its checks run is brought up to date, in branch mode, and not again once
// its push is in. This checks what the service does: the planner's rule
// (!OnTip || !FullRunning) is in TestPlanLine, and lineUpdateAct only pushes
// when the forge says the head is behind, so a planner that asked for an
// update of an up-to-date head would not show here.
func TestLineBranchFrontBehindWithPendingChecksIsUpdatedOnce(t *testing.T) {
	se := newStackEnv(t)
	se.branchMode()
	se.frontWithRunBehind()
	se.line()
	if u := se.sg.updatedPRs(); !slices.Equal(u, []int{7}) {
		t.Fatalf("branches updated %v: want #7 once", u)
	}
	if len(se.made) != 0 {
		t.Errorf("candidates made in branch mode: %v", se.madeHeads())
	}
	// The push: a new head on the tip, its run pending.
	const newHead = "7777777bbbb"
	p := bsPR
	p.HeadSHA = newHead
	se.pullHook("synchronize", p)
	se.s.Wait()
	se.drain()
	se.s.Wait()
	se.sg.behind = nil
	se.sg.setChecks(newHead, CommitCheck{Name: "ci / backend (pull_request)", State: CheckPending, ID: 21}, CommitCheck{Name: "ci / e2e (pull_request)", State: CheckPending, ID: 22})
	se.poll()
	se.s.Wait()
	se.step()
	se.line()
	se.line()
	if u := se.sg.updatedPRs(); !slices.Equal(u, []int{7}) {
		t.Errorf("branches updated %v: the new run's front was updated again", u)
	}
}

// A front on the tip whose full run is going is left alone in branch mode:
// that run is its test. The service reads nothing but the forge's answer
// here (lineUpdateAct is a no-op on an up-to-date head); the rule itself is
// guarded by TestPlanLine's branch front cases.
func TestLineBranchFrontOnTipWithRunGoingIsNotUpdated(t *testing.T) {
	se := newStackEnv(t)
	se.branchMode()
	se.frontWithRunBehind()
	se.sg.behind = nil
	se.line()
	se.step()
	se.line()
	if u := se.sg.updatedPRs(); len(u) != 0 {
		t.Errorf("branches updated %v: want none", u)
	}
	if len(se.made) != 0 {
		t.Errorf("candidates made in branch mode: %v", se.madeHeads())
	}
}

// Candidate mode (the default) leaves a front behind its base with pending
// checks alone: planLine's branch rule (front.Branch) doesn't apply, so no
// push to its branch. It joined green, got a candidate (main + #7), and that
// candidate's run is its test; being behind the base later changes nothing.
func TestLineCandidateModeDoesNotUpdateFrontBehindWithPendingChecks(t *testing.T) {
	se := newStackEnv(t)
	se.draftGreen(head7s, 5)
	se.step()
	se.line()
	if got := strings.Join(se.madeHeads(), " "); got != "7" {
		t.Fatalf("candidates made: %q, want the front's own", got)
	}
	se.stackRun(0, CheckPending, "", "7 "+head7s)
	se.sg.behind = map[string]bool{head7s: true}
	se.line()
	se.step()
	se.line()
	if u := se.sg.updatedPRs(); len(u) != 0 {
		t.Errorf("branches updated %v: want none in candidate mode", u)
	}
	if len(se.made) != 1 {
		t.Errorf("candidates made again: %v", se.madeHeads())
	}
}

// WP-686: a pull request labelled urgent after it joined goes ahead of the
// ones not marked ready, and a later urgent one goes behind the one already
// marked ready (lineSort: holder, fixer, urgent, armed order).
func TestLineLateUrgentLabelOrder(t *testing.T) {
	se := newStackEnv(t)
	se.branchMode()
	head10 := "aaaaaaa0000"
	order := func() []int {
		t.Helper()
		ms, err := se.s.lineMembers(se.ctx, lineRef{forge: se.pr(7).ForgeID, repo: se.pr(7).Repo, base: "main"})
		if err != nil {
			t.Fatal(err)
		}
		// lineOrder sorts ms in place (lineSort); the result is read from ms.
		if _, _, err := se.s.lineOrder(se.ctx, ms, ""); err != nil {
			t.Fatal(err)
		}
		var ns []int
		for _, p := range ms {
			ns = append(ns, p.Number)
		}
		return ns
	}
	se.draftGreen(head7s, 5)
	se.draftGreen(head8s, 7)
	se.draftGreen(head9s, 9)
	se.step()
	if got := order(); !slices.Equal(got, []int{7, 8, 9}) {
		t.Fatalf("joined order %v", got)
	}
	// The line marks #7 ready: it holds the front.
	se.line()
	if pr := se.pr(7); !queueHolder(pr) {
		t.Fatalf("#7 not marked ready: %+v", pr)
	}
	se.labelUrgent(9)
	if got := order(); !slices.Equal(got, []int{7, 9, 8}) {
		t.Errorf("after #9 is labelled urgent: %v, want [7 9 8] (the holder first, then urgent)", got)
	}
	// A later urgent #10 goes behind the holder and ahead of the rest.
	se.queuedDraft(10, "add-more", head10)
	se.draftGreen(head10, 11)
	se.step()
	se.labelUrgent(10)
	if got := order(); !slices.Equal(got, []int{7, 9, 10, 8}) {
		t.Errorf("after urgent #10 joins: %v, want [7 9 10 8]", got)
	}
	// Without the line's own order the same pull requests are told so.
	ahead, err := se.s.queueAhead(se.ctx, se.pr(8))
	if err != nil {
		t.Fatal(err)
	}
	var ns []int
	for _, p := range ahead {
		ns = append(ns, p.Number)
	}
	if !slices.Equal(ns, []int{7, 9, 10}) {
		t.Errorf("ahead of #8: %v, want [7 9 10]", ns)
	}
}
