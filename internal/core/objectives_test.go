package core

import (
	"strings"
	"testing"
	"time"
)

// "Until this runs in under a second": Waypoint reads where it starts, then
// every round's work, on the person's runner. A round that falls short
// opens the next; the round whose reading meets the target closes the task.
// What the worker itself reports decides nothing.
func TestObjectiveUntilAFixedTarget(t *testing.T) {
	o := newObjEnv(t)
	task := o.task("Make the core tests fast")
	ob := o.set(task.Key, ObjectiveInput{Check: ptrOf("go test ./internal/core"), Until: ptrOf("< 1s")})
	if ob.State != ObjectiveActive || ob.Kind != "until" || ob.Measure != MeasureTime || ob.Every != 0 || ob.Round != 1 || ob.Repo != "me/app" {
		t.Fatalf("objective = %+v", ob)
	}
	if want := "until `go test ./internal/core` runs in under 1s"; ob.Summary != want {
		t.Errorf("summary = %q, want %q", ob.Summary, want)
	}
	if next, wait := o.queued(task.Key); next || !strings.Contains(wait, "reading where its objective starts") {
		t.Fatalf("before the baseline: next %v, waits %q", next, wait)
	}
	j := o.read(ObjectiveJobResult{Head: shaA, Reading: timed(2.1)})
	if j.Base != "" || j.Check != "go test ./internal/core" || j.Measure != MeasureTime || j.Round != 1 || !strings.Contains(j.Repo, "me/app") {
		t.Fatalf("baseline job = %+v", j)
	}
	ob = o.obj(task.Key)
	if ob.Phase != PhaseWork || ob.BaseSHA != shaA || ob.Verdict == nil || ob.Verdict.Source != SourceBaseline || ob.Health != "ready" {
		t.Fatalf("after the baseline = %+v (verdict %+v)", ob, ob.Verdict)
	}
	if next, wait := o.queued(task.Key); !next {
		t.Fatalf("round 1 isn't handed out: %q", wait)
	}

	if _, err := o.s.AgentTake(o.ctx, o.ag, task.Key, ""); err != nil {
		t.Fatal(err)
	}
	res, err := o.s.RecordObjectiveCheck(o.ctx, o.ag, task.Key, ObjectiveCheckInput{OK: true, Value: ptrOf(0.001), Runs: 1})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Message, "It decides nothing") {
		t.Errorf("the worker's own reading says %q", res.Message)
	}
	o.setClock(o.clockNow().Add(time.Minute))
	done, err := o.s.AgentDone(o.ctx, o.ag, task.Key, AgentDoneInput{})
	if err != nil {
		t.Fatal(err)
	}
	if done.Finished || !strings.Contains(done.Message, "Waypoint is reading it now") {
		t.Fatalf("done = %+v", done)
	}
	if ob = o.obj(task.Key); ob.Phase != PhaseVerdict || ob.State != ObjectiveActive {
		t.Fatalf("after the round's work = %s %s", ob.State, ob.Phase)
	}
	if next, wait := o.queued(task.Key); next || !strings.Contains(wait, "Waypoint is reading it") {
		t.Fatalf("while it is read: next %v, waits %q", next, wait)
	}
	j = o.read(ObjectiveJobResult{Head: shaB, Base: shaA, Reading: timed(1.4), BaseReading: timed(2.1), P: ptrOf(0.002)})
	if j.Base != shaA {
		t.Fatalf("round 1's job compares with %q", j.Base)
	}
	ob = o.obj(task.Key)
	if ob.Round != 2 || ob.Phase != PhaseWork || ob.BaseSHA != shaB || ob.Stalled != 0 || len(ob.Rounds) != 1 || ob.Rounds[0].Outcome != RoundProgress {
		t.Fatalf("after round 1 = round %d phase %q base %s stalled %d rounds %+v", ob.Round, ob.Phase, ob.BaseSHA, ob.Stalled, ob.Rounds)
	}
	if got := o.status(task.Key); got != "To Do" {
		t.Fatalf("status after round 1 = %s", got)
	}
	hasAll(t, "round 1's reading", ob.Verdict.Line, "1.4s", "33% lower")

	o.worked(task.Key)
	o.read(ObjectiveJobResult{Head: shaC, Base: shaB, Reading: timed(0.8), BaseReading: timed(1.4), P: ptrOf(0.001)})
	ob = o.obj(task.Key)
	if ob.State != ObjectiveMet || len(ob.Rounds) != 2 || ob.Rounds[1].Outcome != RoundMet {
		t.Fatalf("after round 2 = %s %+v", ob.State, ob.Rounds)
	}
	if got := o.status(task.Key); got != "Done" {
		t.Fatalf("status after it is met = %s", got)
	}
	if told := o.told(NotifyObjective); len(told) != 1 || !strings.Contains(told[0], "met") {
		t.Errorf("told = %q", told)
	}
}

// The worker can't close its own task: a reading it reports as met, with
// nothing behind it, changes nothing, and Waypoint's own reading of the
// round (not met) opens the next round.
func TestObjectiveIgnoresWhatTheWorkerReports(t *testing.T) {
	o := newObjEnv(t)
	task := o.task("Fast")
	o.set(task.Key, ObjectiveInput{Check: ptrOf("./bench.sh"), Until: ptrOf("< 1s")})
	o.read(ObjectiveJobResult{Head: shaA, Reading: timed(3)})
	o.take1(task.Key)
	res, err := o.s.RecordObjectiveCheck(o.ctx, o.ag, task.Key, ObjectiveCheckInput{OK: true, Value: ptrOf(0.001)})
	if err != nil || !res.Met {
		t.Fatalf("the working reading = %+v, %v", res, err)
	}
	if ob := o.obj(task.Key); ob.Verdict == nil || ob.Verdict.Source != SourceBaseline || ob.Last.Source != SourceAgent {
		t.Fatalf("verdict %+v, last %+v", ob.Verdict, ob.Last)
	}
	o.setClock(o.clockNow().Add(time.Minute))
	done, err := o.s.AgentDone(o.ctx, o.ag, task.Key, AgentDoneInput{})
	if err != nil || done.Finished {
		t.Fatalf("done = %+v, %v", done, err)
	}
	if got := o.status(task.Key); got == "Done" {
		t.Fatal("the worker's own reading closed the task")
	}
	if _, err := o.s.AgentDone(o.ctx, o.ag, task.Key, AgentDoneInput{}); !IsInvalid(err) {
		t.Errorf("a second done while it is read: %v", err)
	}
	if _, err := o.s.AgentTake(o.ctx, o.ag, task.Key, ""); !IsInvalid(err) {
		t.Errorf("taking it while it is read: %v", err)
	}
	o.read(ObjectiveJobResult{Head: shaB, Base: shaA, Reading: timed(2.9), BaseReading: timed(3), P: ptrOf(0.6)})
	ob := o.obj(task.Key)
	if ob.State != ObjectiveActive || ob.Round != 2 || ob.Rounds[0].Outcome != RoundNone || ob.Stalled != 1 {
		t.Fatalf("after Waypoint's reading: %s round %d %+v stalled %d", ob.State, ob.Round, ob.Rounds[0], ob.Stalled)
	}
	if got := o.status(task.Key); got != "To Do" {
		t.Fatalf("status = %s", got)
	}
}

// Rounds that move nothing wait longer each time; a round that gains
// clears the back-off. The wait holds whatever status the task is in, and
// a session nobody watches can't take the task early by naming it.
func TestObjectiveBacksOffWhenRoundsMoveNothing(t *testing.T) {
	o := newObjEnv(t)
	task := o.task("Coverage")
	o.set(task.Key, ObjectiveInput{Check: ptrOf("./coverage.sh"), Until: ptrOf(">= 80%")})
	val := func(v float64) *ObjectiveReading { return &ObjectiveReading{OK: true, Value: ptrOf(v), Runs: 3} }
	o.read(ObjectiveJobResult{Head: shaA, Reading: val(61)})

	o.worked(task.Key)
	o.read(ObjectiveJobResult{Head: shaB, Base: shaA, Reading: val(61), BaseReading: val(61), Exact: true})
	ob := o.obj(task.Key)
	if ob.Stalled != 1 || ob.NextAt == nil || ob.NextAt.Sub(o.clockNow()) != time.Hour || ob.Rounds[0].Outcome != RoundNone || ob.Health != "waiting" {
		t.Fatalf("after a round that moved nothing: stalled %d next %v %+v %s", ob.Stalled, ob.NextAt, ob.Rounds[0], ob.Health)
	}
	if next, wait := o.queued(task.Key); next || !strings.HasPrefix(wait, WaitObjective+": Round 2") {
		t.Fatalf("queue: next %v, %q", next, wait)
	}
	ip := "In Progress"
	if _, err := o.s.UpdateIssue(o.ctx, o.ag, task.Key, IssuePatch{Status: &ip}); err != nil {
		t.Fatal(err)
	}
	if next, wait := o.queued(task.Key); next || !strings.HasPrefix(wait, WaitObjective) {
		t.Fatalf("In Progress, queue: next %v, %q", next, wait)
	}
	todo := "To Do"
	if _, err := o.s.UpdateIssue(o.ctx, o.admin, task.Key, IssuePatch{Status: &todo}); err != nil {
		t.Fatal(err)
	}
	o.worked(task.Key)
	o.read(ObjectiveJobResult{Head: shaC, Base: shaB, Reading: val(61), BaseReading: val(61), Exact: true})
	if ob = o.obj(task.Key); ob.Stalled != 2 || ob.NextAt.Sub(o.clockNow()) != 2*time.Hour {
		t.Fatalf("after two: stalled %d next %v", ob.Stalled, ob.NextAt)
	}
	if ob, err := o.s.ObjectiveGo(o.ctx, o.admin, task.Key, ObjectiveNext); err != nil || ob.NextAt != nil {
		t.Fatalf("next: %+v, %v", ob, err)
	}
	if next, wait := o.queued(task.Key); !next {
		t.Fatalf("after the person ended the wait: %q", wait)
	}
	o.worked(task.Key)
	o.read(ObjectiveJobResult{Head: shaD, Base: shaC, Reading: val(70), BaseReading: val(61), Exact: true})
	if ob = o.obj(task.Key); ob.Stalled != 0 || ob.NextAt != nil || ob.Round != 4 || ob.Rounds[2].Outcome != RoundProgress {
		t.Fatalf("after a gain: stalled %d next %v round %d", ob.Stalled, ob.NextAt, ob.Round)
	}
}

// "Twice as fast, every round" is never met for good: a round that meets it
// still leaves the task open, a day apart by default, until a person closes
// it. A timed change the runs can't tell from noise isn't a gain.
func TestObjectiveEachRoundNeverEnds(t *testing.T) {
	o := newObjEnv(t)
	task := o.task("Faster search")
	ob := o.set(task.Key, ObjectiveInput{Check: ptrOf("./bench-search"), Until: ptrOf("2x")})
	if ob.Kind != "each" || ob.Every != 24*3600 {
		t.Fatalf("objective = %+v", ob)
	}
	o.read(ObjectiveJobResult{Head: shaA, Reading: timed(1.0)})
	if ob = o.obj(task.Key); ob.State != ObjectiveActive || ob.Phase != PhaseWork {
		t.Fatalf("after the baseline: %s %s", ob.State, ob.Phase)
	}
	o.worked(task.Key)
	o.read(ObjectiveJobResult{Head: shaB, Base: shaA, Reading: timed(0.4), BaseReading: timed(1.0), P: ptrOf(0.001)})
	ob = o.obj(task.Key)
	if ob.State != ObjectiveActive || ob.Round != 2 || ob.Stalled != 0 || ob.NextAt == nil || ob.NextAt.Sub(o.clockNow()) != 24*time.Hour || ob.Rounds[0].Outcome != RoundMet {
		t.Fatalf("after a met round: %s round %d next %v %+v", ob.State, ob.Round, ob.NextAt, ob.Rounds)
	}
	o.setClock(o.clockNow().Add(25 * time.Hour))
	o.worked(task.Key)
	o.read(ObjectiveJobResult{Head: shaC, Base: shaB, Reading: timed(0.36), BaseReading: timed(0.4), P: ptrOf(0.4)})
	if ob = o.obj(task.Key); ob.Rounds[1].Outcome != RoundNone || ob.Stalled != 1 {
		t.Fatalf("noise counted as a gain: %+v", ob.Rounds[1])
	}
	done := "Done"
	if _, err := o.s.UpdateIssue(o.ctx, o.admin, task.Key, IssuePatch{Status: &done}); err != nil {
		t.Fatal(err)
	}
	if ob = o.obj(task.Key); o.status(task.Key) != "Done" || ob.State != ObjectiveEnded || !strings.Contains(ob.EndedWhy, "closed as Done by") {
		t.Fatalf("after the person's close: %s, %q", ob.State, ob.EndedWhy)
	}
}

// A loop has nothing to read: every round opens the task again at once,
// until its rounds run out and the task closes.
func TestObjectiveLoopRunsItsRounds(t *testing.T) {
	o := newObjEnv(t)
	task := o.task("Triage new bug reports")
	ob := o.set(task.Key, ObjectiveInput{Every: ptrOf("0"), MaxRounds: ptrOf(2)})
	if ob.Kind != "loop" || ob.Every != 0 || ob.MaxRounds != 2 || ob.Phase != PhaseWork || ob.Repo != "" {
		t.Fatalf("objective = %+v", ob)
	}
	o.noJob()
	if res := o.worked(task.Key); res.Finished || res.Status != "To Do" {
		t.Fatalf("round 1: %+v", res)
	}
	if next, wait := o.queued(task.Key); !next {
		t.Fatalf("round 2 isn't handed out: %q", wait)
	}
	if res := o.worked(task.Key); !res.Finished {
		t.Fatalf("round 2 of 2: %+v", res)
	}
	ob = o.obj(task.Key)
	if o.status(task.Key) != "Done" || ob.State != ObjectiveEnded || len(ob.Rounds) != 2 || ob.Rounds[1].Outcome != RoundLoop {
		t.Fatalf("after its rounds: %s %+v", ob.State, ob.Rounds)
	}
	if _, err := o.s.SetObjective(o.ctx, o.admin, task.Key, ObjectiveInput{Check: ptrOf("true"), Until: ptrOf("pass")}); !IsInvalid(err) {
		t.Errorf("an objective on a closed task: %v", err)
	}
}

// A round in which nothing landed is still owed: it doesn't use up one of
// the objective's rounds, and it is opened again after a wait.
func TestObjectiveRoundWithNothingLandedIsStillOwed(t *testing.T) {
	o := newObjEnv(t)
	task := o.task("Lint clean")
	o.set(task.Key, ObjectiveInput{Check: ptrOf("make lint"), Until: ptrOf("pass"), MaxRounds: ptrOf(2)})
	o.read(ObjectiveJobResult{Head: shaA, Reading: passed(false)})
	for n := 1; n <= 3; n++ {
		o.worked(task.Key)
		if j := o.read(ObjectiveJobResult{Head: shaA, Base: shaA, Reading: passed(false)}); j.Base != shaA {
			t.Fatalf("job base = %q", j.Base)
		}
		ob := o.obj(task.Key)
		if ob.State != ObjectiveActive || ob.Round != 1 || ob.Phase != PhaseWork || ob.Stalled != n || ob.Rounds[len(ob.Rounds)-1].Outcome != RoundEmpty {
			t.Fatalf("try %d: %s round %d phase %q stalled %d %+v", n, ob.State, ob.Round, ob.Phase, ob.Stalled, ob.Rounds)
		}
		if ob.NextAt == nil {
			t.Fatalf("try %d: no wait before the round is handed out again", n)
		}
		o.setClock(ob.NextAt.Add(time.Minute))
	}
	if told := o.told(NotifyObjective); len(told) != 1 || !strings.Contains(told[0], "3 rounds") {
		t.Errorf("told = %q", told)
	}
}

// Out of rounds without being met, an objective with a target doesn't
// close its task as if it were done: it waits for a person, who gives it
// more rounds or ends it.
func TestObjectiveOutOfRoundsWaitsForAPerson(t *testing.T) {
	o := newObjEnv(t)
	task := o.task("Lint clean")
	o.set(task.Key, ObjectiveInput{Check: ptrOf("make lint"), Until: ptrOf("pass"), MaxRounds: ptrOf(1)})
	o.read(ObjectiveJobResult{Head: shaA, Reading: passed(false)})
	o.worked(task.Key)
	o.read(ObjectiveJobResult{Head: shaB, Base: shaA, Reading: passed(false)})
	ob := o.obj(task.Key)
	if ob.State != ObjectiveActive || ob.Phase != PhaseBlocked || ob.PhaseWhy != BlockedRounds || ob.Health != "blocked" || ob.Rounds[0].Outcome != RoundWorked {
		t.Fatalf("out of rounds: %s %s %s %+v", ob.State, ob.Phase, ob.PhaseWhy, ob.Rounds)
	}
	if o.status(task.Key) == "Done" {
		t.Fatal("the task closed unmet")
	}
	if next, wait := o.queued(task.Key); next || !strings.Contains(wait, "needs a person") {
		t.Fatalf("queue: %v %q", next, wait)
	}
	if told := o.told(NotifyObjective); len(told) != 1 || !strings.Contains(told[0], "needs you") {
		t.Errorf("told = %q", told)
	}
	for _, act := range []string{ObjectiveRetry, ObjectiveNext} {
		if _, err := o.s.ObjectiveGo(o.ctx, o.admin, task.Key, act); !IsInvalid(err) {
			t.Errorf("%s out of rounds: %v", act, err)
		}
	}
	if ob = o.set(task.Key, ObjectiveInput{MaxRounds: ptrOf(3)}); ob.Phase != PhaseWork || ob.Round != 2 || ob.BaseSHA != shaB {
		t.Fatalf("with more rounds: phase %q round %d base %s", ob.Phase, ob.Round, ob.BaseSHA)
	}
	o.worked(task.Key)
	o.read(ObjectiveJobResult{Head: shaC, Base: shaB, Reading: passed(true)})
	if ob = o.obj(task.Key); ob.State != ObjectiveMet || o.status(task.Key) != "Done" {
		t.Fatalf("met in round 2: %s", ob.State)
	}
}

// What already meets the objective when it is set closes the task without
// a round being worked.
func TestObjectiveMetWhereItStarts(t *testing.T) {
	o := newObjEnv(t)
	task := o.task("Lint clean")
	o.set(task.Key, ObjectiveInput{Check: ptrOf("make lint"), Until: ptrOf("pass")})
	o.read(ObjectiveJobResult{Head: shaA, Reading: passed(true)})
	ob := o.obj(task.Key)
	if ob.State != ObjectiveMet || o.status(task.Key) != "Done" || len(ob.Rounds) != 0 {
		t.Fatalf("met at the start: %s, %s, %+v", ob.State, o.status(task.Key), ob.Rounds)
	}
}
