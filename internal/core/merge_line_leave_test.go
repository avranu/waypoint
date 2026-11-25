package core

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// leaveDuringStatus runs a babysit's leave from another goroutine when the
// stack status is posted, then waits a moment to see whether it got through.
type leaveDuringStatus struct {
	*shipForge
	s           *Service
	pr          *PullRequest
	started     atomic.Bool
	done        chan struct{}
	doneInCall  atomic.Bool
	ctxForLeave context.Context
}

func (f *leaveDuringStatus) SetCommitStatus(ctx context.Context, fg *Forge, repo, sha string, st CommitStatusPost) error {
	if st.Context == stackContext && f.started.CompareAndSwap(false, true) {
		go func() {
			f.s.leaveQueue(f.ctxForLeave, f.pr)
			close(f.done)
		}()
		select {
		case <-f.done:
			f.doneInCall.Store(true)
		case <-time.After(100 * time.Millisecond):
		}
	}
	return f.shipForge.SetCommitStatus(ctx, fg, repo, sha, st)
}

// A babysit that takes the front out while the line's step is inside its
// forge calls waits for the step: the step finishes what it started
// (WP-1706), and the leave follows.
func TestLeaveQueueWaitsForTheStepsForgeCalls(t *testing.T) {
	se := newStackEnv(t)
	se.draftGreen(head7s, 5)
	se.step()
	se.line()
	se.stackRun(0, CheckSuccess, treeA, "7 "+head7s)
	se.sg.trees = map[string]string{"3e7000000": treeA}
	pr := se.pr(7)
	f := &leaveDuringStatus{shipForge: se.sg, s: se.s, pr: pr, done: make(chan struct{}), ctxForLeave: context.Background()}
	se.s.Forges = f
	se.line()
	if !f.started.Load() {
		t.Fatal("the step never posted the stack status")
	}
	if f.doneInCall.Load() {
		t.Error("the leave went through while the step held the line's lock")
	}
	select {
	case <-f.done:
	case <-time.After(10 * time.Second):
		t.Fatal("the leave never finished after the step (deadlock?)")
	}
	// The step's marking is read from the forge's record of the title edit
	// that makes the draft ready, a call after the status post: once the
	// leave lands the line puts the draft back, which can happen before the
	// pull request is read here.
	marked := false
	for _, title := range f.edits() {
		marked = marked || !draftTitle(title)
	}
	if !marked {
		t.Error("the step didn't mark the front ready on its candidate after posting its status")
	}
	if p := se.pr(7); p.QueuedAt != nil {
		t.Errorf("the leave didn't take effect after the step: queued_at %v", p.QueuedAt)
	}
}

// Outside any step, leaveQueue just clears queued_at.
func TestLeaveQueueClearsQueuedAt(t *testing.T) {
	se := newStackEnv(t)
	se.draftGreen(head7s, 5)
	se.step()
	se.line()
	pr := se.pr(7)
	if pr.QueuedAt == nil {
		t.Fatal("not queued to begin with")
	}
	se.s.leaveQueue(se.ctx, pr)
	if p := se.pr(7); p.QueuedAt != nil {
		t.Errorf("queued_at = %v", p.QueuedAt)
	}
}

// A context that holds the line's lock passes through leaveQueue and a second
// queueLockCtx; the inner unlock doesn't release the outer lock.
func TestQueueLockCtxIsReentrant(t *testing.T) {
	se := newStackEnv(t)
	se.draftGreen(head7s, 5)
	se.step()
	se.line()
	pr := se.pr(7)
	held, unlock := se.s.queueLockCtx(se.ctx, pr)
	finished := func(f func()) bool {
		done := make(chan struct{})
		go func() { f(); close(done) }()
		select {
		case <-done:
			return true
		case <-time.After(300 * time.Millisecond):
			return false
		}
	}
	if !finished(func() { se.s.leaveQueue(held, pr) }) {
		unlock()
		t.Fatal("leaveQueue on a ctx that holds the lock waited on itself")
	}
	if p := se.pr(7); p.QueuedAt != nil {
		t.Error("the leave under the lock didn't clear queued_at")
	}
	if !finished(func() {
		_, inner := se.s.queueLockCtx(held, pr)
		inner()
	}) {
		unlock()
		t.Fatal("a second queueLockCtx waited on the first")
	}
	got := make(chan struct{})
	go func() {
		_, u := se.s.queueLockCtx(context.Background(), pr)
		close(got)
		u()
	}()
	select {
	case <-got:
		t.Fatal("the inner unlock released the outer lock")
	case <-time.After(150 * time.Millisecond):
	}
	// Another line's key isn't covered by this one's.
	other := *pr
	other.BaseRef = "elsewhere"
	if !finished(func() { _, u := se.s.queueLockCtx(held, &other); u() }) {
		t.Error("another line's lock was skipped or blocked")
	}
	unlock()
	select {
	case <-got:
	case <-time.After(5 * time.Second):
		t.Fatal("the outer unlock didn't release the lock")
	}
	// A context that crossed to another goroutine holds nothing.
	if _, u := se.s.queueLockCtx(withoutLineLocks(held), pr); u == nil {
		t.Fatal("no unlock")
	} else {
		u()
	}
}

// A babysit round under a holder (readyCatchUp holds the lock while a
// conflict round parks) leaves the queue on the holder's ctx without waiting
// on its own lock.
func TestParkUnderTheQueueLockDoesNotDeadlock(t *testing.T) {
	be, pr, _ := parkedAboutChecks(t)
	b := be.babysit()
	held, unlock := be.s.queueLockCtx(be.ctx, pr)
	defer unlock()
	done := make(chan struct{})
	go func() {
		r := lastRound(b)
		be.s.parkBabysit(held, b, pr, "its ledger is spent", "", r.Checks, r.Conflict, r.Comments, r.Findings)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("parkBabysit waited on the lock its caller holds")
	}
}

// A marker is good only while its holder holds: a context kept past the
// unlock locks for real.
func TestQueueLockCtxMarkerEndsWithTheLock(t *testing.T) {
	se := newStackEnv(t)
	se.draftGreen(head7s, 5)
	pr := se.pr(7)
	stale, unlock := se.s.queueLockCtx(se.ctx, pr)
	unlock()
	_, again := se.s.queueLockCtx(stale, pr) // takes the lock
	got := make(chan func())
	go func() {
		_, u := se.s.queueLockCtx(context.Background(), pr)
		got <- u
	}()
	select {
	case u := <-got:
		u()
		again()
		t.Fatal("a stale marker skipped the lock")
	case <-time.After(150 * time.Millisecond):
	}
	again()
	select {
	case u := <-got:
		u()
	case <-time.After(5 * time.Second):
		t.Fatal("unlocking didn't release the lock")
	}
}
