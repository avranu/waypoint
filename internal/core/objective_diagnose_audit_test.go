package core

import (
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Round 3 of WP-1449's review (audit 1214): what a diagnose round must not
// let anyone finish or ship, what a re-set leaves behind, and the words said.

func (o *objEnv) toldBodies(kind string) []string {
	o.t.Helper()
	o.s.Wait()
	ns, _, err := o.s.ListNotifications(o.ctx, o.admin, false, 50)
	if err != nil {
		o.t.Fatal(err)
	}
	var out []string
	for _, n := range ns {
		if n.Kind == kind {
			out = append(out, n.Title+" | "+n.Body)
		}
	}
	return out
}

// toDiagnoseValue is toDiagnose for a measured value, which can make
// progress: two rounds that came no nearer, so round 3 is a diagnose round.
func (o *objEnv) toDiagnoseValue() *IssueDetail {
	o.t.Helper()
	task := o.task("Coverage")
	o.set(task.Key, ObjectiveInput{Check: ptrOf("./coverage.sh"), Until: ptrOf(">= 99%"), DiagnoseAfter: ptrOf(2)})
	val := func(v float64) *ObjectiveReading { return &ObjectiveReading{OK: true, Value: ptrOf(v), Runs: 3} }
	o.read(ObjectiveJobResult{Head: sha(0), Reading: val(50)})
	for n := 1; n <= 2; n++ {
		o.worked(task.Key)
		o.read(ObjectiveJobResult{Head: sha(n), Base: sha(n - 1), Reading: val(50), BaseReading: val(50), Exact: true})
	}
	if ob := o.obj(task.Key); ob.RoundKind != RoundKindDiagnose {
		o.t.Fatalf("setup: round %d is %q", ob.Round, ob.RoundKind)
	}
	return task
}

// .2: a proposal's rev doesn't change for an objective on the default.
func TestObjectiveRevOmitsTheDefaultDiagnoseAfter(t *testing.T) {
	o := newObjEnv(t)
	task := o.task("Lint clean")
	ob := o.set(task.Key, ObjectiveInput{Check: ptrOf("make lint"), Until: ptrOf("pass")})
	if ob.DiagnoseAfter != objectiveDiagnoseDefault {
		t.Fatalf("diagnose_after = %d", ob.DiagnoseAfter)
	}
	before := objectiveHash(append(ob.measured(), ob.Pace, strconv.FormatInt(ob.everyMS, 10), strconv.Itoa(ob.MaxRounds)))
	if got := ob.rev(); got != before {
		t.Errorf("the rev of a default objective is %s, want the one without the field, %s", got, before)
	}
	ob = o.set(task.Key, ObjectiveInput{DiagnoseAfter: ptrOf(5)})
	if got := ob.rev(); got == before {
		t.Errorf("a non-default diagnose_after leaves the rev as %s", got)
	}
	if ob = o.set(task.Key, ObjectiveInput{DiagnoseAfter: ptrOf(objectiveDiagnoseDefault)}); ob.rev() != before {
		t.Errorf("back on the default: rev %s, want %s", ob.rev(), before)
	}
}

// .4: a check changed while it is out of rounds leaves no stale base for
// the rounds a later raise opens.
func TestObjectiveDiagnoseOutOfRoundsCheckChangeClearsTheBase(t *testing.T) {
	o := newObjEnv(t)
	task := o.toDiagnose(2, 2, ObjectiveInput{MaxRounds: ptrOf(2)})
	if ob := o.obj(task.Key); ob.PhaseWhy != BlockedRounds || ob.BaseSHA == "" {
		t.Fatalf("setup: why %q base %q", ob.PhaseWhy, ob.BaseSHA)
	}
	ob := o.set(task.Key, ObjectiveInput{Check: ptrOf("make lint-all")})
	if ob.PhaseWhy != BlockedRounds || ob.BaseSHA != "" {
		t.Fatalf("after the check changed: why %q base %q", ob.PhaseWhy, ob.BaseSHA)
	}
	ob = o.set(task.Key, ObjectiveInput{MaxRounds: ptrOf(9)})
	if ob.Round != 3 || ob.Phase != PhaseBaseline || ob.BaseSHA != "" {
		t.Fatalf("after the rounds were raised: round %d phase %q base %q, want a baseline reading first", ob.Round, ob.Phase, ob.BaseSHA)
	}
}

// .5a: wp done doesn't finish a diagnose round on any path.
func TestObjectiveDiagnoseDoneRefusedOnEveryPath(t *testing.T) {
	t.Run("a linked pull request that hasn't merged", func(t *testing.T) {
		o := newObjEnv(t)
		task := o.toDiagnose(2, 2, ObjectiveInput{})
		o.take1(task.Key)
		o.pull(task.Key, 7)
		_, err := o.s.AgentDone(o.ctx, o.ag, task.Key, AgentDoneInput{Comment: "done"})
		if !IsInvalid(err) || !strings.Contains(err.Error(), "wp objective diagnose") {
			t.Fatalf("wp done with a pull request open: %v", err)
		}
		if d := o.get(o.admin, task.Key); d.ClaimedBy == "" {
			t.Error("the refused wp done released the claim")
		}
	})
	t.Run("a workflow with a later step", func(t *testing.T) {
		o := newObjEnv(t)
		task := o.toDiagnose(2, 2, ObjectiveInput{})
		if _, err := o.s.CreateWorkflow(o.ctx, o.admin, o.p.Key, WorkflowInput{Name: str("Reviewed"), Steps: &[]WorkflowStep{{Name: "implement"}, {Name: "review"}}}); err != nil {
			t.Fatal(err)
		}
		if _, err := o.s.UpdateIssue(o.ctx, o.admin, task.Key, IssuePatch{Workflow: Opt[string]{Set: true, Value: str("Reviewed")}}); err != nil {
			t.Fatal(err)
		}
		o.take1(task.Key)
		_, err := o.s.AgentDone(o.ctx, o.ag, task.Key, AgentDoneInput{})
		if !IsInvalid(err) || !strings.Contains(err.Error(), "wp objective diagnose") {
			t.Fatalf("wp done at a step with another after it: %v", err)
		}
		if d := o.get(o.admin, task.Key); d.Step != "implement" {
			t.Errorf("the step moved to %q", d.Step)
		}
	})
}

// .6b: a pull request isn't opened for a diagnose round, by agent or person.
func TestObjectiveDiagnoseRefusesOpeningAPullRequest(t *testing.T) {
	for name, who := range map[string]func(o *objEnv) *Actor{
		"an agent": func(o *objEnv) *Actor { return o.ag },
		"a person": func(o *objEnv) *Actor { return o.admin },
	} {
		t.Run(name, func(t *testing.T) {
			o := newObjEnv(t)
			task := o.toDiagnose(2, 2, ObjectiveInput{})
			for _, in := range []OpenPRInput{
				{Project: o.p.Key, Head: "round", Title: "Round work", Issue: task.Key},
				{Project: o.p.Key, Head: "round", Title: "Round work", For: task.Key},
			} {
				_, err := o.s.OpenPullRequest(o.ctx, who(o), in)
				if !IsInvalid(err) {
					t.Fatalf("opening for %+v: %v", in, err)
				}
				hasAll(t, "the refusal", err.Error(), "wp objective diagnose "+task.Key, "reopens as a work round", "wp objective next "+task.Key)
			}
		})
	}
}

// .7: a diagnose round nobody has taken up is told to be taken first.
func TestObjectiveDiagnoseDoneBeforeTakenSaysTake(t *testing.T) {
	o := newObjEnv(t)
	task := o.toDiagnose(2, 2, ObjectiveInput{})
	o.take1(task.Key)
	if _, err := o.s.AgentRelease(o.ctx, o.ag, task.Key, ReleaseInput{}); err != nil {
		t.Fatal(err)
	}
	o.s.Wait()
	done := "Done"
	_, err := o.s.UpdateIssue(o.ctx, o.ag, task.Key, IssuePatch{Status: &done})
	if !IsInvalid(err) || !strings.Contains(err.Error(), "take it first (wp next "+task.Key+")") {
		t.Fatalf("closing a diagnose round no one holds: %v", err)
	}
	_, err = o.s.AgentDone(o.ctx, o.ag, task.Key, AgentDoneInput{})
	if !IsInvalid(err) || !strings.Contains(err.Error(), "take it first (wp next "+task.Key+")") {
		t.Fatalf("wp done on a diagnose round no one holds: %v", err)
	}
}

// .8: out of rounds after a diagnose round that was read leaves no round kind.
func TestObjectiveDiagnoseMergedLastRoundClearsTheKind(t *testing.T) {
	o := newObjEnv(t)
	task := o.toDiagnose(2, 2, ObjectiveInput{MaxRounds: ptrOf(3)})
	o.take1(task.Key)
	url := o.pull(task.Key, 7)
	o.linkState(url, "merged")
	o.read(ObjectiveJobResult{Head: sha(3), Base: sha(2), Reading: passed(false)})
	ob := o.obj(task.Key)
	if ob.PhaseWhy != BlockedRounds || ob.RoundKind != "" {
		t.Fatalf("phase %q why %q kind %q", ob.Phase, ob.PhaseWhy, ob.RoundKind)
	}
}

// .13: an ended objective has no round kind; the count and stall stay as the record.
func TestObjectiveDiagnoseEndClearsTheKind(t *testing.T) {
	o := newObjEnv(t)
	task := o.toDiagnose(2, 2, ObjectiveInput{})
	ob, err := o.s.ObjectiveGo(o.ctx, o.admin, task.Key, ObjectiveEnd)
	if err != nil || ob.State != ObjectiveEnded {
		t.Fatalf("end: %+v, %v", ob, err)
	}
	if ob.RoundKind != "" || ob.NoBetter != 2 || ob.Stalled != 2 {
		t.Fatalf("kind %q no_better %d stalled %d, want '' 2 2", ob.RoundKind, ob.NoBetter, ob.Stalled)
	}
}

// .9: with diagnosing off, the refusal says so and how to turn it on.
func TestObjectiveDiagnoseOffRefusalSaysHowToTurnItOn(t *testing.T) {
	o := newObjEnv(t)
	task := o.toDiagnose(0, 2, ObjectiveInput{})
	o.take1(task.Key)
	_, err := o.diagnose(task.Key, ObjectiveDiagnoseInput{Text: "x"})
	if !IsInvalid(err) {
		t.Fatalf("diagnosing a work round: %v", err)
	}
	hasAll(t, "the refusal", err.Error(), "diagnose rounds are off", "wp objective set "+task.Key+" --diagnose-after")
	if strings.Contains(err.Error(), "owed after") {
		t.Errorf("it says a diagnosis is owed: %v", err)
	}
}

// .11: taken off after N rounds counts the rounds worked, not the diagnoses.
func TestObjectiveDiagnoseClearDoesNotCountDiagnoses(t *testing.T) {
	o := newObjEnv(t)
	task := o.toDiagnose(2, 2, ObjectiveInput{})
	o.take1(task.Key)
	if _, err := o.diagnose(task.Key, ObjectiveDiagnoseInput{Text: "found"}); err != nil {
		t.Fatal(err)
	}
	if err := o.s.ClearObjective(o.ctx, o.admin, task.Key); err != nil {
		t.Fatal(err)
	}
	h := o.history(task.Key)
	if last := h[len(h)-1]; !strings.Contains(last, "after 2 rounds") {
		t.Fatalf("the history says %q, want it taken off after 2 rounds", last)
	}
}

// .14 and .15: the brief clips a long diagnosis and says whose account it is.
func TestObjectiveDiagnoseBriefClipsAndHedges(t *testing.T) {
	o := newObjEnv(t)
	task := o.toDiagnose(2, 2, ObjectiveInput{})
	o.take1(task.Key)
	long := strings.Repeat("The search is slow because of the index. ", 450) + "THE-END-MARKER"
	ob, err := o.diagnose(task.Key, ObjectiveDiagnoseInput{Text: long})
	if err != nil {
		t.Fatal(err)
	}
	brief := ObjectiveText(ob, true, o.clockNow())
	if strings.Contains(brief, "THE-END-MARKER") || len(brief) > 12000 {
		t.Errorf("the brief isn't clipped (%d bytes)", len(brief))
	}
	hasAll(t, "the brief", brief, "[the rest is cut: wp objective show "+task.Key+" has all of it]", "not something Waypoint checked", "check it against the readings")
	// A short one is whole.
	ob.Diagnosis.Text = "The check times the build."
	if brief = ObjectiveText(ob, true, o.clockNow()); !strings.Contains(brief, "The check times the build.") || strings.Contains(brief, "the rest is cut") {
		t.Errorf("a short diagnosis was cut:\n%s", brief)
	}
	// The limit is characters, not bytes: 5,000 three-byte characters fit.
	ob.Diagnosis.Text = strings.Repeat("€", 5000) + "WIDE-END"
	if brief = ObjectiveText(ob, true, o.clockNow()); !strings.Contains(brief, "WIDE-END") || strings.Contains(brief, "the rest is cut") {
		t.Errorf("a diagnosis under the limit in characters was cut (%d bytes)", len(brief))
	}
	ob.Diagnosis.Text = strings.Repeat("€", objectiveBriefDiagnosis+1)
	if brief = ObjectiveText(ob, true, o.clockNow()); !strings.Contains(brief, strings.Repeat("€", objectiveBriefDiagnosis)+"\n") || !strings.Contains(brief, "the rest is cut") {
		t.Errorf("a diagnosis one character over isn't cut at the limit")
	}
}

// .18: the stall notice says a diagnosis stands and how many rounds read since
// came no nearer.
func TestObjectiveDiagnoseStallNoticeNamesTheDiagnosis(t *testing.T) {
	o := newObjEnv(t)
	task := o.toDiagnose(2, 2, ObjectiveInput{})
	o.take1(task.Key)
	if _, err := o.diagnose(task.Key, ObjectiveDiagnoseInput{Text: "the check times the build"}); err != nil {
		t.Fatal(err)
	}
	o.noBetter(task.Key, 3)
	got := o.stallNotice()
	if got == "" {
		t.Fatalf("no stall notice: %q", o.toldBodies(NotifyObjective))
	}
	hasAll(t, "the stall notice", got, "A diagnosis was recorded in round 3", "since then Waypoint has read 1 round that came no nearer")
}

// .20: a loop has no rounds to read, so diagnose_after is refused on one.
func TestObjectiveDiagnoseAfterOnALoopIsRefused(t *testing.T) {
	o := newObjEnv(t)
	task := o.task("Tidy")
	_, err := o.s.SetObjective(o.ctx, o.admin, task.Key, ObjectiveInput{Every: ptrOf("0"), DiagnoseAfter: ptrOf(1)})
	if !IsInvalid(err) || !strings.Contains(err.Error(), "a loop has no rounds to read") {
		t.Fatalf("diagnose_after on a loop: %v", err)
	}
	// Left out, it is a loop on the default, and never diagnoses.
	ob := o.set(task.Key, ObjectiveInput{Every: ptrOf("0")})
	if ob.Kind != "loop" {
		t.Fatalf("kind %q", ob.Kind)
	}
	// A judge or a check makes it rounds that are read: then it may be set.
	if _, err := o.s.SetObjective(o.ctx, o.admin, task.Key, ObjectiveInput{Check: ptrOf("make lint"), Until: ptrOf("pass"), DiagnoseAfter: ptrOf(1)}); err != nil {
		t.Fatalf("diagnose_after with a check: %v", err)
	}
}

// .32: a check changed in the rounds drops the diagnosis recorded for the old one.
func TestObjectiveDiagnoseCheckChangeDropsTheDiagnosis(t *testing.T) {
	o := newObjEnv(t)
	task := o.toDiagnose(2, 2, ObjectiveInput{})
	o.take1(task.Key)
	if ob, err := o.diagnose(task.Key, ObjectiveDiagnoseInput{Text: "the check times the build"}); err != nil || ob.Diagnosis == nil {
		t.Fatalf("setup: %+v, %v", ob, err)
	}
	ob := o.set(task.Key, ObjectiveInput{Check: ptrOf("make lint-all")})
	if ob.Diagnosis != nil || ob.NoBetter != 0 || ob.RoundKind != "" {
		t.Fatalf("diagnosis %v no_better %d kind %q", ob.Diagnosis, ob.NoBetter, ob.RoundKind)
	}
}

// .33: something other than a person closing the task in a diagnose round
// leaves it open, and says so.
func TestObjectiveDiagnoseNonDirectCloseKeepsItOpen(t *testing.T) {
	o := newObjEnv(t)
	task := o.toDiagnose(2, 2, ObjectiveInput{})
	o.take1(task.Key)
	done := "Done"
	if _, err := o.s.UpdateIssue(o.ctx, SystemActor(), task.Key, IssuePatch{Status: &done}); err != nil {
		t.Fatal(err)
	}
	o.s.Wait()
	if d := o.get(o.admin, task.Key); d.Closed() {
		t.Fatalf("the task closed: %s", d.Status.Name)
	}
	found := false
	for _, h := range o.history(task.Key) {
		found = found || strings.Contains(h, "a diagnose round: it stays open until a diagnosis is recorded")
	}
	if !found {
		t.Errorf("no note: %q", o.history(task.Key))
	}
	if ob := o.obj(task.Key); ob.RoundKind != RoundKindDiagnose || ob.Phase != PhaseWork {
		t.Errorf("the round moved: %q %q", ob.RoundKind, ob.Phase)
	}
}

// .34: a judged objective diagnoses after rounds the judge reads as no nearer.
func TestObjectiveDiagnoseJudged(t *testing.T) {
	o := newObjEnv(t)
	task := o.task("Export the board")
	o.set(task.Key, ObjectiveInput{Judge: ptrOf("The board exports as CSV."), DiagnoseAfter: ptrOf(2)})
	o.read(ObjectiveJobResult{Head: shaA})
	o.judged("Nothing yet.", finding("high", "There is no CSV export"))
	for i, head := range []string{shaB, shaC} {
		o.worked(task.Key)
		o.read(ObjectiveJobResult{Head: head})
		o.judged("Still not.", finding("high", fmt.Sprintf("There is still no CSV export %d", i)))
	}
	ob := o.obj(task.Key)
	if ob.RoundKind != RoundKindDiagnose || ob.NoBetter != 2 {
		t.Fatalf("kind %q no_better %d after two rounds the judge found no nearer", ob.RoundKind, ob.NoBetter)
	}
	hasAll(t, "the brief", ObjectiveText(ob, true, o.clockNow()), "THIS ROUND IS A DIAGNOSE ROUND", "the check or the judge itself")
}

// .35: a person diagnosing over an agent's held round takes it from the agent.
func TestObjectiveDiagnosePersonOverAnAgentsHeldRound(t *testing.T) {
	o := newObjEnv(t)
	task := o.toDiagnose(2, 2, ObjectiveInput{})
	o.take1(task.Key)
	if d := o.get(o.admin, task.Key); d.ClaimedBy == "" {
		t.Fatal("setup: not held")
	}
	if _, err := o.s.ObjectiveDiagnose(o.ctx, o.admin, task.Key, ObjectiveDiagnoseInput{Text: "the person's own"}); err != nil {
		t.Fatal(err)
	}
	d := o.get(o.admin, task.Key)
	if d.ClaimedBy != "" || d.ClaimedUntil != nil {
		t.Errorf("still claimed: %q until %v", d.ClaimedBy, d.ClaimedUntil)
	}
	// Runs come newest first: the first is the held round's, ended as diagnosed.
	if len(d.AgentRuns) == 0 || d.AgentRuns[0].EndedAt == nil || d.AgentRuns[0].Outcome != RunDiagnosed {
		t.Errorf("the agent's run wasn't ended as diagnosed: %+v", d.AgentRuns)
	}
	for _, r := range d.AgentRuns {
		if r.EndedAt == nil {
			t.Errorf("a run is still open: %+v", r)
		}
	}
}

// .36: a loop's no-better count stays 0 as well as its round kind.
func TestObjectiveDiagnoseLoopCountsNothing(t *testing.T) {
	o := newObjEnv(t)
	task := o.task("Tidy")
	o.set(task.Key, ObjectiveInput{Every: ptrOf("0")})
	for i := 0; i < 3; i++ {
		o.worked(task.Key)
		if ob := o.obj(task.Key); ob.RoundKind != "" || ob.NoBetter != 0 {
			t.Fatalf("round %d of a loop: kind %q no_better %d", ob.Round, ob.RoundKind, ob.NoBetter)
		}
	}
}

// .37: a person's next from a diagnose round that ended without being read
// clears the count, and a diagnosis already recorded stands.
func TestObjectiveDiagnoseNextFromVerdictKeepsTheDiagnosis(t *testing.T) {
	o := newObjEnv(t)
	task := o.toDiagnose(2, 2, ObjectiveInput{})
	o.take1(task.Key)
	if _, err := o.diagnose(task.Key, ObjectiveDiagnoseInput{Text: "the check times the build"}); err != nil {
		t.Fatal(err)
	}
	o.noBetter(task.Key, 3)
	o.noBetter(task.Key, 4)
	o.take1(task.Key)
	url := o.pull(task.Key, 7)
	o.linkState(url, "merged")
	ob := o.obj(task.Key)
	if ob.Phase != PhaseVerdict || ob.RoundKind != RoundKindDiagnose || ob.Diagnosis == nil {
		t.Fatalf("setup: phase %q kind %q diagnosis %v", ob.Phase, ob.RoundKind, ob.Diagnosis)
	}
	ob, err := o.s.ObjectiveGo(o.ctx, o.admin, task.Key, ObjectiveNext)
	if err != nil {
		t.Fatal(err)
	}
	if ob.NoBetter != 0 || ob.Diagnosis == nil || ob.Diagnosis.Text != "the check times the build" {
		t.Fatalf("no_better %d diagnosis %v", ob.NoBetter, ob.Diagnosis)
	}
}

// .39: the note for a pull request that landed in a diagnose round, whatever it read as.
func TestObjectiveDiagnosePullRequestLandedNote(t *testing.T) {
	for name, reading := range map[string]float64{"progress": 60, "met": 99.5} {
		t.Run(name, func(t *testing.T) {
			o := newObjEnv(t)
			task := o.toDiagnoseValue()
			o.take1(task.Key)
			url := o.pull(task.Key, 7)
			o.linkState(url, "merged")
			val := func(v float64) *ObjectiveReading { return &ObjectiveReading{OK: true, Value: ptrOf(v), Runs: 3} }
			o.read(ObjectiveJobResult{Head: sha(3), Base: sha(2), Reading: val(reading), BaseReading: val(50), Exact: true})
			found := false
			for _, h := range o.history(task.Key) {
				found = found || strings.Contains(h, "it was a diagnose round, but a pull request landed in it")
			}
			if !found {
				t.Errorf("no note in %q", o.history(task.Key))
			}
			ob := o.obj(task.Key)
			if name == "met" && ob.State != ObjectiveMet || name == "progress" && (ob.NoBetter != 0 || ob.RoundKind != "") {
				t.Errorf("state %q no_better %d kind %q", ob.State, ob.NoBetter, ob.RoundKind)
			}
		})
	}
}

// .40: a diagnosis without --ask tells no one.
func TestObjectiveDiagnoseWithoutAskTellsNoOne(t *testing.T) {
	for name, who := range map[string]*func(o *objEnv) *Actor{
		"an agent": ptrOf(func(o *objEnv) *Actor { return o.ag }),
		"a person": ptrOf(func(o *objEnv) *Actor { return o.admin }),
	} {
		t.Run(name, func(t *testing.T) {
			o := newObjEnv(t)
			task := o.toDiagnose(2, 2, ObjectiveInput{})
			o.take1(task.Key)
			before := o.toldBodies(NotifyObjective)
			if _, err := o.s.ObjectiveDiagnose(o.ctx, (*who)(o), task.Key, ObjectiveDiagnoseInput{Text: "found"}); err != nil {
				t.Fatal(err)
			}
			if after := o.toldBodies(NotifyObjective); len(after) != len(before) {
				t.Fatalf("told %q, then %q", before, after)
			}
		})
	}
}

// .3: lowering diagnose_after takes effect when the next round opens: a
// round a worker may hold isn't turned into a diagnose round under them.
func TestObjectiveDiagnoseLoweringTakesEffectNextRound(t *testing.T) {
	o := newObjEnv(t)
	task := o.toDiagnose(3, 2, ObjectiveInput{})
	if ob := o.obj(task.Key); ob.RoundKind != "" || ob.NoBetter != 2 {
		t.Fatalf("setup: kind %q no_better %d", ob.RoundKind, ob.NoBetter)
	}
	o.take1(task.Key)
	ob := o.set(task.Key, ObjectiveInput{DiagnoseAfter: ptrOf(2)})
	if ob.RoundKind != "" {
		t.Fatalf("a round that is held became a %q round", ob.RoundKind)
	}
	o.setClock(o.clockNow().Add(time.Minute))
	if _, err := o.s.AgentDone(o.ctx, o.ag, task.Key, AgentDoneInput{}); err != nil {
		t.Fatal(err)
	}
	o.s.Wait()
	o.read(ObjectiveJobResult{Head: sha(3), Base: sha(2), Reading: passed(false)})
	if ob = o.obj(task.Key); ob.RoundKind != RoundKindDiagnose {
		t.Fatalf("the next round is a %q round", ob.RoundKind)
	}
}
