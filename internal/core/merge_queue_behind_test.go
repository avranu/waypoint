package core

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

// warmQueue opens drafts #7..#last, all armed with green draft tiers: #7 is
// marked ready (the holder), the rest queue behind it in order. It returns
// each one's head.
func (be *bsEnv) warmQueue(last int) map[int]string {
	be.t.Helper()
	heads := map[int]string{}
	for n := 7; n <= last; n++ {
		heads[n] = fmt.Sprintf("%07daaaa", n)
		be.queuedDraft(n, fmt.Sprintf("branch-%d", n), heads[n])
	}
	for n := 7; n <= last; n++ {
		be.draftChecks(heads[n], CommitCheck{State: CheckSuccess, ID: int64(100 + 2*n), Description: "Successful in 4m"}, CommitCheck{State: CheckSuccess, ID: int64(101 + 2*n), Description: "Has been skipped"})
	}
	return heads
}

// mergeFront passes #7's full run so that it merges; main moves to tip-2.
// The pull requests in behind are behind main from the moment it moves.
func (be *bsEnv) mergeFront(head string, behind ...string) {
	be.t.Helper()
	be.sg.onMerge = func() {
		be.sg.baseTip = "tip-2"
		be.sg.behind = map[string]bool{}
		for _, h := range behind {
			be.sg.behind[h] = true
		}
	}
	be.sg.trees = map[string]string{"4e7ced0ffee": "t7", head: "t7"}
	be.draftChecks(head, CommitCheck{State: CheckSuccess, ID: 1000, Description: "Successful in 16m"}, CommitCheck{State: CheckSuccess, ID: 1001, Description: "Successful in 24m"})
	if m := be.sg.merged(); len(m) != 1 {
		be.t.Fatalf("merges %+v", m)
	}
}

// After a merge only the new front is brought up to date with main; the
// drafts behind it get no push and so no draft-tier run (WP-510): the next
// merge would move main again before their turn.
func TestMergeQueueUpdatesOnlyTheFront(t *testing.T) {
	be := newBabysitEnvOn(t, "forgejo")
	be.sg.files[".forgejo/workflows/ci.yml"] = ciOnEdit
	be.runnerOnline()
	heads := be.warmQueue(12)
	if pr := be.pr(7); pr.State != "open" || pr.ReadyAt == nil {
		t.Fatalf("#7 not marked ready: %+v", pr)
	}
	be.mergeFront(heads[7], heads[8], heads[9], heads[10], heads[11], heads[12])
	be.sweep()
	if u := be.sg.updatedPRs(); !slices.Equal(u, []int{8}) {
		t.Fatalf("updates %v: want #8, the front, only", u)
	}
	// The front is marked ready before its update, so the update runs the
	// full tier (WP-456).
	if pr := be.pr(8); pr.State != "open" || pr.ReadyAt == nil {
		t.Errorf("#8, the front, not marked ready for its update: %+v", pr)
	}
	for n := 9; n <= 12; n++ {
		if pr := be.pr(n); pr.State != "draft" || pr.ReadyAt != nil || pr.QueuedAt == nil {
			t.Errorf("#%d: %+v", n, pr)
		}
		w := be.babysitOf(n).Wait
		if want := fmt.Sprintf("in the merge queue for main: %s, behind #8", ordinal(n-7)); !strings.HasPrefix(w, want) || strings.Contains(w, "merged main") || strings.Contains(w, "brought up to date") {
			t.Errorf("#%d waits %q", n, w)
		}
	}
	// Further steps push nothing to them either.
	be.sweep()
	be.sweep()
	if u := be.sg.updatedPRs(); !slices.Equal(u, []int{8}) {
		t.Fatalf("updates %v after more sweeps", u)
	}
}

// A draft right behind the front that comes to conflict with main is told
// by the forge's mergeable flag, read again after the merge with no push and
// no CI, and its conflict goes to a round at once, while the front's full
// run goes (WP-457): it keeps its place, and when the front merges its turn
// is a clean update, not a round. GitHub reports the flag for its drafts;
// Forgejo says nothing for a work-in-progress title (WP-171), so there a
// stack's run says it (TestStackedQueueBaseConflictRoundsEarly) or the
// front's update does (TestMergeQueueForgejoDraftsNotReread).
func TestMergeQueueConflictBehindTheFrontFromTheFlag(t *testing.T) {
	be := newBabysitEnvOn(t, "github")
	be.sg.files[".github/workflows/ci.yml"] = ciOnEdit
	be.runnerOnline()
	heads := be.warmQueue(11)
	// #7 merges; the forge now finds #9 and #11 conflict with the new main.
	be.fg.mu.Lock()
	for _, n := range []int{9, 10, 11} {
		be.fg.prs[mk(be.f, "me/app", n)].State = "draft"
	}
	be.fg.mu.Unlock()
	be.forgeSays(9, ptr(false))
	be.forgeSays(11, ptr(false))
	be.mergeFront(heads[7], heads[8], heads[9], heads[10], heads[11])
	be.sweep()
	// The forge is asked again soon, not at the next sync.
	n, at := be.rechecks(9)
	if at == nil || at.Sub(be.clockNow()) != mergeConfirmAfter {
		t.Fatalf("#9's reread: %d %v (now %v)", n, at, be.clockNow())
	}
	be.setClock(be.clockNow().Add(mergeConfirmAfter))
	be.s.CheckMergeRechecks(be.ctx)
	be.s.Wait()
	// The "no" stands on a read a while later (WP-138).
	for _, d := range mergeRecheckDelays[:2] {
		be.setClock(be.clockNow().Add(d))
		be.s.CheckMergeRechecks(be.ctx)
		be.s.Wait()
	}
	be.runnerOnline() // it kept checking in while the clock jumped
	be.sweep()
	if u := be.sg.updatedPRs(); !slices.Equal(u, []int{8}) {
		t.Fatalf("updates %v: want #8, the front, only", u)
	}
	b := be.babysitOf(9)
	if b.Round != 1 || !b.Rounds[0].Conflict || b.JobID == 0 {
		t.Fatalf("#9, 2nd, got no conflict round behind the front: %+v", b)
	}
	if pr := be.pr(9); pr.State != "draft" || pr.QueuedAt == nil {
		t.Errorf("#9 lost its place: %+v", pr)
	}
	if pr := be.pr(8); pr.ReadyAt == nil {
		t.Errorf("#8 lost the front: %+v", pr)
	}
	// 4th is past conflictEarly: its round waits for its turn.
	if b := be.babysitOf(11); b.Round != 0 || !strings.Contains(b.Wait, "4th") || !strings.Contains(b.Wait, "has conflicts with main, resolved by a round when its turn at the front comes") {
		t.Errorf("#11: %+v (waits %q)", b, b.Wait)
	}

	// The round merges main into #9 and pushes; the forge finds it merges.
	be.finish(b.JobID, true)
	p9, err := be.fg.GetPR(be.ctx, be.f, "me/app", 9)
	if err != nil {
		t.Fatal(err)
	}
	be.forgeSays(9, ptr(true))
	resolved := be.pushTo(*p9, "9999999eeee")
	be.draftChecks(resolved.HeadSHA, CommitCheck{State: CheckSuccess, ID: 3000, Description: "Successful in 4m"}, CommitCheck{State: CheckSuccess, ID: 3001, Description: "Has been skipped"})

	// #8's update comes in and passes its full run: it merges, and #9's turn
	// is the update, cleanly, with no second round.
	p8, err := be.fg.GetPR(be.ctx, be.f, "me/app", 8)
	if err != nil {
		t.Fatal(err)
	}
	moved := be.pushTo(*p8, "8888888dddd")
	be.sg.onMerge = func() {
		be.sg.baseTip = "tip-3"
		be.sg.behind = map[string]bool{resolved.HeadSHA: true, heads[10]: true, heads[11]: true}
	}
	be.sg.trees = map[string]string{"4e7ced0ffee": "t8", moved.HeadSHA: "t8"}
	be.draftChecks(moved.HeadSHA, CommitCheck{State: CheckSuccess, ID: 2000, Description: "Successful in 16m"}, CommitCheck{State: CheckSuccess, ID: 2001, Description: "Successful in 24m"})
	if m := be.sg.merged(); len(m) != 2 {
		t.Fatalf("merges %+v (#8 waits %q)", m, be.babysitOf(8).Wait)
	}
	be.runnerOnline()
	be.sweep()
	if m := be.sg.merged(); len(m) != 2 {
		t.Fatalf("merges %+v: #9 is the front", m)
	}
	if b := be.babysitOf(9); b.Round != 1 {
		t.Fatalf("#9 at the front took another round: %+v", b)
	}
	if u := be.sg.updatedPRs(); !slices.Contains(u, 9) {
		t.Errorf("updates %v: want #9 brought up to date at its turn (waits %q)", u, be.babysitOf(9).Wait)
	}
}

// Forgejo answers no for every draft titled work in progress (WP-171): a
// merge doesn't have it asked about the drafts behind the front.
func TestMergeQueueForgejoDraftsNotReread(t *testing.T) {
	be := newBabysitEnvOn(t, "forgejo")
	be.sg.files[".forgejo/workflows/ci.yml"] = ciOnEdit
	be.runnerOnline()
	heads := be.warmQueue(9)
	be.mergeFront(heads[7], heads[8], heads[9])
	be.sweep()
	if _, at := be.rechecks(9); at != nil {
		t.Errorf("#9 to be read again at %v", at)
	}
}
