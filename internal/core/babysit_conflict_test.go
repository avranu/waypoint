package core

import (
	"strings"
	"testing"
	"time"
)

// A head's rounds climb the ladder (WP-439): its own model, the stronger
// one, the assessor, then a rest out of the merge line (WP-930). A conflict
// that arose after a resting head's rounds (the base moved) waits with it:
// the rest's end, or a push, sends it on, and the next round has the
// conflict.
func TestBabysitConflictAfterRoundOnSameHead(t *testing.T) {
	be := newBabysitEnv(t)
	be.runnerOnline()
	pr := be.open()
	be.start(pr)
	be.ci("7777777aaaa", CheckFailed)
	be.finish(be.babysit().JobID, false)
	b := be.babysit()
	if b.State != BabysitWorking || b.Round != 2 || b.Rounds[1].Model != escalateModel || b.Rounds[0].Conflict {
		t.Fatalf("after a round with nothing pushed: %+v", b)
	}
	// The stronger round changes nothing either: the assessor.
	be.finish(b.JobID, false)
	if b = be.babysit(); b.State != BabysitWorking || b.Round != 3 || !b.Rounds[2].Assessor || b.Rounds[2].Conflict {
		t.Fatalf("after the stronger round changed nothing: %+v", b)
	}
	// Nor does the assessor: it rests out of the merge line, nobody asked.
	be.finish(b.JobID, false)
	if b = be.babysit(); b.State != BabysitResting || !resting(&b.Rounds[2]) {
		t.Fatalf("after the assessor changed nothing: %+v", b)
	}
	// Main moves; the forge repeats its "no" well after the head.
	be.forgeSays(7, ptr(false))
	be.readForge(7)
	be.setClock(be.clockNow().Add(mergeCheckingLimit + time.Minute)) // still "no" long after: a conflict
	be.runnerOnline()
	be.step()
	if b = be.babysit(); !resting(lastRound(b)) || len(be.jobs()) != 3 {
		t.Fatalf("resting, a round ran for the conflict: %+v jobs %v", b, be.jobs())
	}
	// The rest ends: one more assessor round, told nobody was asked, with
	// the conflict and every way to end.
	be.endRest()
	b = be.babysit()
	if b.Round != 4 || !b.Rounds[3].Conflict || !b.Rounds[3].Assessor || b.Rounds[2].Reply == "" || len(be.jobs()) != 4 {
		t.Fatalf("after the rest: %+v jobs %v", b, be.jobs())
	}
	if in := be.jobInput(b.JobID); !strings.Contains(in.Prompt, "Nobody was asked") || !strings.Contains(in.Prompt, "`Question: …` and `Because: …`") {
		t.Fatalf("assessor after the rest:\n%s", in.Prompt)
	}
	// Never twice: nothing more is handed out while it runs.
	be.step()
	if len(be.jobs()) != 4 {
		t.Fatalf("%d jobs", len(be.jobs()))
	}
	// It had the conflict and left it: it rests again. The conflict is new
	// since the last rest, so this one is no longer than a first.
	be.finish(b.JobID, false)
	b = be.babysit()
	if !resting(&b.Rounds[3]) || !strings.Contains(b.Rounds[3].Parked, "round 4") || len(be.jobs()) != 4 {
		t.Fatalf("after a round that left the conflict: %+v", b)
	}
	if got := b.Rounds[3].RestUntil.Sub(be.clockNow()); got != restFirst {
		t.Errorf("rest over something new %v, want %v", got, restFirst)
	}
	var asked int
	_ = be.s.db.R.QueryRow(`SELECT count(*) FROM requests WHERE tool_use_id LIKE ?`, parkRequestKey+"%").Scan(&asked)
	if asked != 0 {
		t.Errorf("%d park questions, want none", asked)
	}
}

// endRest moves the clock past #7's rest and steps, and when that re-ran its
// checks, past the wait for their results, which are the same.
func (be *bsEnv) endRest() {
	be.t.Helper()
	r := lastRound(be.babysit())
	if !resting(r) {
		return
	}
	be.setClock(r.RestUntil.Add(time.Second))
	be.runnerOnline()
	be.step()
	if r = lastRound(be.babysit()); resting(r) && !r.RerunAt.IsZero() {
		be.setClock(be.clockNow().Add(faultRerunSettle))
		be.runnerOnline()
		be.step()
	}
}
