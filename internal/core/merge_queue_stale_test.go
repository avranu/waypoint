package core

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

// reviewRunning turns the review gate on and puts a review of the pull
// request's head in progress: its gate is pending until reviewDone.
func (be *bsEnv) reviewRunning(n int) {
	be.t.Helper()
	pr := be.pr(n)
	if _, err := be.s.db.W.ExecContext(be.ctx, `UPDATE project_repos SET review_gate = 1`); err != nil {
		be.t.Fatal(err)
	}
	if _, err := be.s.db.W.ExecContext(be.ctx, `INSERT INTO bot_runs(project_id, pr_id, kind, head_sha, status, created_at)
		VALUES ((SELECT min(id) FROM projects), ?, 'review', ?, 'running', ?)`, pr.ID, pr.HeadSHA, be.s.nowMS()); err != nil {
		be.t.Fatal(err)
	}
	if err := be.s.evalGate(be.ctx, pr.ID); err != nil {
		be.t.Fatal(err)
	}
	be.s.Wait()
}

// reviewDone finishes the reviews of #7 and #8 with no findings.
func (be *bsEnv) reviewDone() {
	be.t.Helper()
	if _, err := be.s.db.W.ExecContext(be.ctx, `UPDATE bot_runs SET status = 'done', finished_at = ? WHERE status = 'running'`, be.s.nowMS()); err != nil {
		be.t.Fatal(err)
	}
	for _, n := range []int{7, 8} {
		if err := be.s.evalGate(be.ctx, be.pr(n).ID); err != nil {
			be.t.Fatal(err)
		}
	}
	be.s.Wait()
}

func (be *bsEnv) sweep() {
	be.t.Helper()
	be.s.CheckBabysits(be.ctx)
	be.s.Wait()
}

// pushTo moves pull request n (p, as opened) to a new head.
func (be *bsEnv) pushTo(p ForgePR, sha string) ForgePR {
	be.t.Helper()
	p.HeadSHA = sha
	be.pullHook("synchronize", p)
	be.s.Wait()
	be.drain()
	be.s.Wait()
	return p
}

var (
	draftTierGreen7 = []CommitCheck{{State: CheckSuccess, ID: 5, Description: "Successful in 4m"}, {State: CheckSuccess, ID: 6, Description: "Has been skipped"}}
	draftTierGreen8 = []CommitCheck{{State: CheckSuccess, ID: 7, Description: "Successful in 4m"}, {State: CheckSuccess, ID: 8, Description: "Has been skipped"}}
	fullRunGreen7   = []CommitCheck{{State: CheckSuccess, ID: 9, Description: "Successful in 16m"}, {State: CheckSuccess, ID: 10, Description: "Successful in 24m"}}
)

// The front of the queue is brought up to date as soon as its base moves,
// while its checks run, not after a full run on a head that can't merge
// (WP-316); the draft behind it is left as it is (WP-510).
func TestMergeQueueFrontUpdatedWhileChecksRun(t *testing.T) {
	be := newBabysitEnvOn(t, "forgejo")
	be.sg.files[".forgejo/workflows/ci.yml"] = ciOnEdit
	be.runnerOnline()
	const head7, head8 = "7777777aaaa", "8888888cccc"
	p7 := be.queuedDraft(7, "add-export", head7)
	be.queuedDraft(8, "add-import", head8)
	be.draftChecks(head7, draftTierGreen7[0], draftTierGreen7[1])
	be.draftChecks(head8, draftTierGreen8[0], draftTierGreen8[1])
	if pr := be.pr(7); pr.State != "open" || pr.ReadyAt == nil {
		t.Fatalf("#7 not marked ready: %+v", pr)
	}

	// #7's full run is going when main moves.
	be.sg.behind = map[string]bool{head7: true, head8: true}
	be.sweep()
	if u := be.sg.updatedPRs(); !slices.Equal(u, []int{7}) {
		t.Fatalf("updates %v: want the front only", u)
	}
	if w := be.babysitOf(7).Wait; !strings.Contains(w, "merged main into it, waiting for its checks") {
		t.Errorf("#7 waits %q", w)
	}
	// Looking again for the same tip updates nothing more.
	be.sweep()
	if u := be.sg.updatedPRs(); len(u) != 1 {
		t.Fatalf("updates %v", u)
	}

	// Its update's head comes in, and main moves again while that one's
	// checks run: updated once more.
	p7 = be.pushTo(p7, "7777777bbbb")
	be.sg.baseTip = "tip-2"
	be.sg.behind[p7.HeadSHA] = true
	be.sweep()
	if u := be.sg.updatedPRs(); !slices.Equal(u, []int{7, 7}) {
		t.Fatalf("updates %v: want #7 updated again while its checks run", u)
	}
	if pr := be.pr(8); pr.State != "draft" || pr.ReadyAt != nil {
		t.Errorf("#8 moved: %+v", pr)
	}
}

// An update whose new head came in without the tip it was made for is
// updated again at once, up to updateAttempts times for the tip; then the
// usual wait applies (WP-316).
func TestUpdateAgainWhenTheNewHeadLacksTheTip(t *testing.T) {
	be := newBabysitEnvOn(t, "forgejo")
	be.sg.files[".forgejo/workflows/ci.yml"] = ciOnEdit
	be.runnerOnline()
	const head7 = "7777777aaaa"
	p := be.queuedDraft(7, "add-export", head7)
	be.sg.behind = map[string]bool{head7: true}
	be.draftChecks(head7, draftTierGreen7[0], draftTierGreen7[1])
	if n := be.sg.updated(); n != 1 {
		t.Fatalf("updates %d", n)
	}
	// Nothing came in yet: waiting.
	be.sweep()
	if n := be.sg.updated(); n != 1 {
		t.Fatalf("updates %d before the push came in", n)
	}
	// Each head that comes in lacks the tip (the update was made against a
	// base that had moved on): updated again, three times in all.
	for i, sha := range []string{"7777777bbbb", "7777777cccc", "7777777dddd"} {
		be.sg.behind[sha] = true
		p = be.pushTo(p, sha)
		be.sweep()
		want := min(i+2, updateAttempts)
		if n := be.sg.updated(); n != want {
			t.Fatalf("after head %s: updates %d, want %d", sha, n, want)
		}
	}
	// A different tip starts again.
	be.sg.baseTip = "tip-2"
	be.sweep()
	if n := be.sg.updated(); n != updateAttempts+1 {
		t.Fatalf("updates %d for a new tip", n)
	}
}

// A base whose read doesn't show Waypoint's last merge yet isn't updated
// with or tested against; once the merge shows, or after staleReadWait, it is.
func TestStaleBaseReadWaitedOut(t *testing.T) {
	be := newBabysitEnvOn(t, "forgejo")
	be.sg.files[".forgejo/workflows/ci.yml"] = ciOnEdit
	be.runnerOnline()
	const head7 = "7777777aaaa"
	be.queuedDraft(7, "add-export", head7)
	pr := be.pr(7)
	be.sg.behind = map[string]bool{head7: true}

	// A merge in flight (the forge hasn't said what it made): wait.
	be.s.noteMerging(pr)
	be.draftChecks(head7, draftTierGreen7[0], draftTierGreen7[1])
	if n := be.sg.updated(); n != 0 {
		t.Fatalf("updated %d times against a base with a merge in flight", n)
	}
	if w := be.babysitOf(7).Wait; !strings.Contains(w, "waiting for main to show the merge just made") {
		t.Errorf("waits %q", w)
	}
	// The merge landed, but the read of main lacks it.
	be.s.noteMerged(pr, "m1m1m1m1")
	be.sg.hidden = map[string]bool{"m1m1m1m1": true}
	be.sweep()
	if n := be.sg.updated(); n != 0 {
		t.Fatalf("updated %d times against a stale read", n)
	}
	// It shows now: the update goes.
	delete(be.sg.hidden, "m1m1m1m1")
	be.sg.baseTip = "tip-2"
	be.sweep()
	if n := be.sg.updated(); n != 1 {
		t.Fatalf("updates %d once the merge shows", n)
	}
	if _, ok := be.s.baseMerged.Load(baseKey(pr)); ok {
		t.Error("the merge is remembered after it showed")
	}

	// A merge that never shows can't hold the queue for ever.
	be.s.noteMerged(pr, "m2m2m2m2")
	be.sg.hidden["m2m2m2m2"] = true
	be.sg.baseTip = "tip-3"
	be.sweep()
	if n := be.sg.updated(); n != 1 {
		t.Fatalf("updates %d inside the wait", n)
	}
	be.setClock(be.clockNow().Add(staleReadWait + time.Second))
	be.sweep()
	if n := be.sg.updated(); n != 2 {
		t.Fatalf("updates %d after the wait", n)
	}
}

// A merge Waypoint makes is remembered per base, and a refused one isn't.
func TestMergeNotesTheBase(t *testing.T) {
	be := newBabysitEnvOn(t, "forgejo")
	be.sg.files[".forgejo/workflows/ci.yml"] = ciOnEdit
	be.runnerOnline()
	const head7 = "7777777aaaa"
	be.queuedDraft(7, "add-export", head7)
	pr := be.pr(7)
	be.draftChecks(head7, draftTierGreen7[0], draftTierGreen7[1])
	be.sg.mergeErr = errors.New("not enough approvals")
	be.draftChecks(head7, fullRunGreen7[0], fullRunGreen7[1])
	be.sweep()
	if _, ok := be.s.baseMerged.Load(baseKey(pr)); ok {
		t.Fatal("a refused merge is remembered")
	}
	be.sg.mergeErr = nil
	if _, err := be.s.db.W.ExecContext(be.ctx, `UPDATE babysits SET state = 'watching' WHERE pr_id = ?`, pr.ID); err != nil {
		t.Fatal(err)
	}
	be.sweep()
	be.sweep()
	if len(be.sg.merged()) != 1 {
		t.Fatalf("merges %+v (babysit %q)", be.sg.merged(), be.babysitOf(7).Wait)
	}
	v, ok := be.s.baseMerged.Load(baseKey(pr))
	if !ok || v.(baseMerge).sha != "4e7ced0ffee" {
		t.Fatalf("the merge isn't remembered: %v", v)
	}
}

// The full run starts as soon as the draft tier is green on the base's tip,
// with the review still going; the merge still waits for the review. A pull
// request behind the front stays a draft either way.
func TestMergeQueueReadyWhileReviewGoes(t *testing.T) {
	be := newBabysitEnvOn(t, "forgejo")
	be.sg.files[".forgejo/workflows/ci.yml"] = ciOnEdit
	be.runnerOnline()
	const head7, head8 = "7777777aaaa", "8888888cccc"
	be.queuedDraft(7, "add-export", head7)
	be.queuedDraft(8, "add-import", head8)
	be.reviewRunning(7)
	be.reviewRunning(8)

	be.draftChecks(head7, draftTierGreen7[0], draftTierGreen7[1])
	be.draftChecks(head8, draftTierGreen8[0], draftTierGreen8[1])
	be.sweep()
	if pr := be.pr(7); pr.State != "open" || pr.ReadyAt == nil {
		t.Fatalf("#7 not marked ready with its review going: %+v (babysit %q)", pr, be.babysitOf(7).Wait)
	}
	if pr := be.pr(8); pr.State != "draft" || pr.ReadyAt != nil {
		t.Fatalf("#8 jumped the queue: %+v", pr)
	}

	// The full run passes while the review is still going: no merge.
	be.draftChecks(head7, fullRunGreen7[0], fullRunGreen7[1])
	be.sweep()
	if m := be.sg.merged(); len(m) != 0 {
		t.Fatalf("merged %+v before the review finished", m)
	}
	// The review finishes: #7 merges.
	be.reviewDone()
	be.sweep()
	be.sweep()
	if m := be.sg.merged(); len(m) != 1 {
		t.Fatalf("merges %+v after the review (babysit %q)", m, be.babysitOf(7).Wait)
	}
}

// A draft front on a head behind its base, review or no, is marked ready
// just before it is brought up to date, so the update's push runs the full
// tier (WP-456): the edit's run on the old head is cancelled by that push,
// and readiness goes by the new head's checks.
func TestReadyEarlyNeedsTheTip(t *testing.T) {
	be := newBabysitEnvOn(t, "forgejo")
	be.sg.files[".forgejo/workflows/ci.yml"] = ciOnEdit
	be.runnerOnline()
	const head7 = "7777777aaaa"
	p7 := be.queuedDraft(7, "add-export", head7)
	be.reviewRunning(7)
	be.sg.behind = map[string]bool{head7: true}
	updatesAtEdit := -1
	be.sg.afterTitleEdit = func(title string) {
		if !draftTitle(title) {
			updatesAtEdit = be.sg.updated()
		}
	}
	be.draftChecks(head7, draftTierGreen7[0], draftTierGreen7[1])
	if n := be.sg.updated(); n != 1 {
		t.Fatalf("updates %d: want it brought up to date", n)
	}
	if updatesAtEdit != 0 {
		t.Fatalf("marked ready after the update (%d updates by then): the update's push ran the draft tier", updatesAtEdit)
	}
	if pr := be.pr(7); pr.State != "open" || pr.ReadyAt == nil {
		t.Fatalf("#7 not marked ready for the update: %+v", pr)
	}
	// The update's head, on the ready title: its own checks decide, not the
	// old head's.
	p7.Title = "Change add-export"
	be.pushTo(p7, "7777777bbbb")
	pr := be.pr(7)
	checks, err := be.s.prChecks(be.ctx, pr)
	if err != nil {
		t.Fatal(err)
	}
	if r := fullRunReasons(pr, checks); len(r) != 0 {
		t.Fatalf("waits on the old head's full run: %v", r)
	}
	if rd, _ := be.s.readiness(be.ctx, pr); rd.Ready {
		t.Fatalf("ready before the update's checks: %+v", rd)
	}
}

// The 16:24 sequence: #8 became the front when #7 gave up its place, was
// updated against a base that had not yet shown #7's merge, and its full
// run went on to the end on a head that could not merge. Now the stale
// read is waited out, and once the base moves the front is updated while its
// checks run, and again if the new head lacks the tip.
func TestMergeQueueStaleBaseSequence(t *testing.T) {
	be := newBabysitEnvOn(t, "forgejo")
	be.sg.files[".forgejo/workflows/ci.yml"] = ciOnEdit
	be.runnerOnline()
	const head7, head8 = "7777777aaaa", "8888888cccc"
	be.queuedDraft(7, "add-export", head7)
	p8 := be.queuedDraft(8, "add-import", head8)
	be.draftChecks(head7, draftTierGreen7[0], draftTierGreen7[1])
	be.draftChecks(head8, draftTierGreen8[0], draftTierGreen8[1])
	// #7's merge is refused: it asks a person and gives up its place, so #8
	// is the front and is marked ready.
	be.sg.mergeErr = errors.New("not enough approvals")
	be.draftChecks(head7, fullRunGreen7[0], fullRunGreen7[1])
	be.sweep()
	be.sweep()
	if pr := be.pr(8); pr.State != "open" || pr.ReadyAt == nil {
		t.Fatalf("#8 not the front: %+v (babysit %q)", pr, be.babysitOf(8).Wait)
	}

	// #7 merges into main (a person approved it) but the forge still reads
	// the old tip: #8 is behind, and the base doesn't hold #7's merge.
	be.s.noteMerged(be.pr(7), "m7m7m7m7")
	be.sg.hidden = map[string]bool{"m7m7m7m7": true}
	be.sg.behind = map[string]bool{head8: true}
	be.sweep()
	if u := be.sg.updatedPRs(); len(u) != 0 {
		t.Fatalf("#8 updated against a base that lacks #7's merge: %v", u)
	}

	// Main shows the merge: #8 is updated while its full run is going.
	delete(be.sg.hidden, "m7m7m7m7")
	be.sg.baseTip = "tip-2"
	be.sweep()
	if u := be.sg.updatedPRs(); !slices.Equal(u, []int{8}) {
		t.Fatalf("updates %v", u)
	}
	// The update's head lacks the tip: updated again at once.
	be.sg.behind["8888888dddd"] = true
	be.pushTo(p8, "8888888dddd")
	be.sweep()
	if u := be.sg.updatedPRs(); !slices.Equal(u, []int{8, 8}) {
		t.Fatalf("updates %v", u)
	}
}
